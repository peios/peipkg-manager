// Package watch detects new upstream versions and emits build triggers.
//
// Two complementary mechanisms feed the same trigger channel:
//
//   - Polling: per-recipe goroutines `git ls-remote --tags` on a
//     configured interval. Cheap (no clone) and always-on, so it
//     catches versions even when webhooks miss.
//
//   - GitHub webhooks: an HTTP endpoint receives push/tag events. On a
//     valid event, the corresponding recipe is poll-checked
//     immediately, so a tag pushed at 14:00 doesn't wait until the
//     next 1-hour tick.
//
// Watch emits a Trigger for every tag that matches a recipe's
// tag_pattern — it does NOT deduplicate against the published
// archive. That is the manager's job: watch is "here is what could be
// built;" manager is "here is what has not yet been built."
package watch

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/peios/peipkg-manager/internal/recipe"
	"github.com/peios/peipkg-manager/internal/source"
	"github.com/peios/peipkg-manager/internal/version"
)

// Trigger reports one (recipe, upstream tag) pair the manager should
// consider building. The same logical version may be reported multiple
// times across triggers (different sources, repeated polls) — the
// manager dedupes.
type Trigger struct {
	Recipe      recipe.Recipe
	UpstreamTag string // e.g., "v1.3.2"
	Captured    string // capture group 1 from tag_pattern, e.g., "1.3.2"
	Source      string // "poll" or "webhook" — for log/audit
}

// Watcher is the long-lived poll+webhook coordinator.
type Watcher struct {
	Recipes       []recipe.Recipe
	DefaultPoll   time.Duration
	WebhookSecret string // empty = webhook signature verification skipped (development only)
	HTTPAddr      string // empty = no HTTP server
	Logger        *slog.Logger
	Triggers      chan<- Trigger

	// StatusHandler, when non-nil, is mounted at /status on the
	// webhook HTTP server. Used by the manager to expose its own
	// state (in-flight builds, failure records, etc.) without making
	// the watcher aware of the manager's internals.
	StatusHandler http.Handler

	// patterns is a per-recipe compiled tag regex, shared across the
	// poll loop and the webhook handler.
	patterns map[string]*regexp.Regexp

	// recipesByGit maps a normalised upstream git URL to a recipe so
	// webhook payloads can dispatch in O(1).
	recipesByGit map[string]recipe.Recipe
}

// Run starts the poll loops and (if HTTPAddr is set) the webhook HTTP
// server. It returns when ctx is cancelled, after waiting for all
// goroutines to drain.
func (w *Watcher) Run(ctx context.Context) error {
	if err := w.compilePatterns(); err != nil {
		return err
	}

	var wg sync.WaitGroup

	for _, r := range w.Recipes {
		if !r.Upstream.HasUpstream() {
			continue
		}
		interval := w.DefaultPoll
		if r.Watch.PollInterval != nil {
			interval = r.Watch.PollInterval.Duration
		}
		wg.Add(1)
		go func(r recipe.Recipe, interval time.Duration) {
			defer wg.Done()
			w.pollLoop(ctx, r, interval)
		}(r, interval)
	}

	if w.HTTPAddr != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.serveHTTP(ctx)
		}()
	}

	wg.Wait()
	return nil
}

// RunOnce polls every recipe with [upstream] config in parallel,
// emits triggers as it finds matching tags, and returns when all
// polls have completed. Used by --once mode; no ticker, no webhook
// server.
//
// The caller is responsible for closing the trigger channel after
// RunOnce returns so any downstream `for range triggers` terminates.
func (w *Watcher) RunOnce(ctx context.Context) error {
	if err := w.compilePatterns(); err != nil {
		return err
	}

	var wg sync.WaitGroup
	for _, r := range w.Recipes {
		if !r.Upstream.HasUpstream() {
			continue
		}
		wg.Add(1)
		go func(r recipe.Recipe) {
			defer wg.Done()
			w.pollOnce(ctx, r)
		}(r)
	}
	wg.Wait()
	return nil
}

// compilePatterns precompiles every recipe's tag_pattern and builds the
// recipesByGit lookup. Done once at Run startup so per-poll work is
// cheap.
func (w *Watcher) compilePatterns() error {
	w.patterns = make(map[string]*regexp.Regexp, len(w.Recipes))
	w.recipesByGit = make(map[string]recipe.Recipe, len(w.Recipes))

	for _, r := range w.Recipes {
		if !r.Upstream.HasUpstream() {
			continue
		}
		re, err := regexp.Compile(r.Upstream.TagPattern)
		if err != nil {
			return fmt.Errorf("recipe %s: invalid tag_pattern %q: %w", r.ID, r.Upstream.TagPattern, err)
		}
		if re.NumSubexp() < 1 {
			return fmt.Errorf("recipe %s: tag_pattern %q must have at least one capture group (the version string)", r.ID, r.Upstream.TagPattern)
		}
		w.patterns[r.ID] = re
		w.recipesByGit[normalizeGitURL(r.Upstream.Git)] = r
	}
	return nil
}

func (w *Watcher) pollLoop(ctx context.Context, r recipe.Recipe, interval time.Duration) {
	w.pollOnce(ctx, r) // initial poll on startup, before the first tick

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.pollOnce(ctx, r)
		}
	}
}

func (w *Watcher) pollOnce(ctx context.Context, r recipe.Recipe) {
	w.Logger.Info("polling upstream", "recipe", r.ID, "git", r.Upstream.Git)

	tags, err := source.ListTags(ctx, r.Upstream.Git)
	if err != nil {
		w.Logger.Warn("ls-remote failed", "recipe", r.ID, "err", err)
		return
	}

	re := w.patterns[r.ID]
	emitted := 0
	skippedOld := 0
	for _, tag := range tags {
		m := re.FindStringSubmatch(tag)
		if len(m) < 2 {
			continue
		}
		captured := m[1]

		if r.Upstream.MinVersion != "" {
			ok, err := version.UpstreamGTE(captured, r.Upstream.MinVersion)
			if err != nil {
				// Validation should be at recipe-load time; if it
				// reaches here, tolerate by treating the tag as
				// in-bounds rather than silently dropping every tag.
				w.Logger.Warn("min_version comparison failed; emitting anyway",
					"recipe", r.ID, "tag", tag, "err", err)
			} else if !ok {
				skippedOld++
				continue
			}
		}

		select {
		case <-ctx.Done():
			return
		case w.Triggers <- Trigger{Recipe: r, UpstreamTag: tag, Captured: captured, Source: "poll"}:
			emitted++
		}
	}
	w.Logger.Info("poll complete", "recipe", r.ID, "tags_seen", len(tags), "matches_emitted", emitted, "skipped_below_min_version", skippedOld)
}

// serveHTTP runs the webhook receiver until ctx is cancelled.
func (w *Watcher) serveHTTP(ctx context.Context) {
	mux := http.NewServeMux()
	mux.HandleFunc("/webhooks/github", w.handleGitHubWebhook)
	mux.HandleFunc("/healthz", func(rw http.ResponseWriter, _ *http.Request) { _, _ = rw.Write([]byte("ok\n")) })
	if w.StatusHandler != nil {
		mux.Handle("/status", w.StatusHandler)
	}

	server := &http.Server{
		Addr:              w.HTTPAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	w.Logger.Info("webhook server listening", "addr", w.HTTPAddr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		w.Logger.Error("webhook server error", "err", err)
	}
}

// handleGitHubWebhook validates a GitHub webhook payload and triggers a
// poll on the affected recipe.
//
// We don't extract the new tag from the payload directly — we just use
// the webhook as a "wake up and poll this recipe right now" signal.
// Polling is robust (it walks the full tag list and tag_pattern), so
// the webhook only needs to identify the recipe; the poll does the
// work of finding the new versions.
func (w *Watcher) handleGitHubWebhook(rw http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Read the body (capped) for HMAC verification.
	body, err := io.ReadAll(io.LimitReader(req.Body, 16*1024*1024))
	if err != nil {
		http.Error(rw, "read body", http.StatusBadRequest)
		return
	}

	if w.WebhookSecret != "" {
		sig := req.Header.Get("X-Hub-Signature-256")
		if !validGitHubSignature(body, sig, w.WebhookSecret) {
			w.Logger.Warn("webhook signature invalid", "remote", req.RemoteAddr)
			http.Error(rw, "invalid signature", http.StatusUnauthorized)
			return
		}
	}

	var payload struct {
		Repository struct {
			HTMLURL  string `json:"html_url"`
			CloneURL string `json:"clone_url"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(rw, "parse json", http.StatusBadRequest)
		return
	}

	// Find which recipe this event belongs to.
	candidates := []string{payload.Repository.HTMLURL, payload.Repository.CloneURL}
	var matched *recipe.Recipe
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if r, ok := w.recipesByGit[normalizeGitURL(c)]; ok {
			matched = &r
			break
		}
	}
	if matched == nil || !matched.Watch.GitHubWebhook {
		// Either we don't have a recipe for this repo or the recipe
		// has explicitly opted out of webhook handling. Either way,
		// 200 OK so GitHub doesn't keep retrying.
		w.Logger.Info("webhook for unrecognised or opted-out repo", "html_url", payload.Repository.HTMLURL)
		rw.WriteHeader(http.StatusOK)
		return
	}

	w.Logger.Info("webhook accepted, triggering immediate poll", "recipe", matched.ID)
	go w.pollOnce(req.Context(), *matched)
	rw.WriteHeader(http.StatusOK)
}

// validGitHubSignature verifies a GitHub-style HMAC SHA-256 signature
// (header `X-Hub-Signature-256: sha256=<hex>`). Empty signature or
// secret are rejected.
func validGitHubSignature(body []byte, signature, secret string) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(signature, prefix) {
		return false
	}
	provided, err := hex.DecodeString(signature[len(prefix):])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(provided, mac.Sum(nil))
}

// LoadSecret reads the webhook secret from a file. Trailing whitespace
// (including the trailing newline that text editors love to add) is
// trimmed; everything else is taken as-is.
func LoadSecret(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read webhook secret %s: %w", path, err)
	}
	return strings.TrimRight(string(data), " \t\r\n"), nil
}

// normalizeGitURL maps a git URL to a comparable form so the same
// repository's HTTPS, HTTPS-with-.git, and clone-URL forms all match.
//
// The algorithm: lowercase, strip ".git" suffix, strip trailing slash.
// Adequate for the GitHub-only v0 case; future protocol prefixes
// (ssh:// vs https://) can extend it.
func normalizeGitURL(s string) string {
	s = strings.ToLower(s)
	s = strings.TrimSuffix(s, "/")
	s = strings.TrimSuffix(s, ".git")
	return s
}
