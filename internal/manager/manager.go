// Package manager is the long-running coordinator that ties config,
// recipes, watching, building, and publishing into one daemon.
//
// Responsibilities:
//
//   - Resolve the peipkg-build and peipkg-repo binaries on PATH at
//     startup and pin them so a later PATH change cannot redirect a
//     running daemon to different binaries.
//   - Initialise the repository state at first run (no operator step
//     between "I installed peipkg-manager" and "I have a repository").
//   - Run a watcher that emits build triggers from polling and webhooks.
//   - Drain the trigger channel: dedupe each trigger against the
//     published archive, build the new ones sequentially, publish each
//     successful build, optionally rclone-sync the result.
//   - Reload the recipe roster on demand (Reload, called from a SIGHUP
//     handler) without restarting the process: cancel the current
//     watcher epoch, re-read recipes_dir, start a fresh watcher epoch.
//     In-flight builds use the outer context and run to completion
//     across reloads.
//   - Shut down cleanly on SIGTERM/SIGINT (cancel context, drain
//     in-flight build, exit).
package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/peios/peipkg-manager/internal/build"
	"github.com/peios/peipkg-manager/internal/config"
	"github.com/peios/peipkg-manager/internal/publish"
	"github.com/peios/peipkg-manager/internal/recipe"
	pkgversion "github.com/peios/peipkg-manager/internal/version"
	"github.com/peios/peipkg-manager/internal/watch"
)

// Manager is the assembled daemon. Construct via New, run via Run.
type Manager struct {
	cfg    config.Config
	logger *slog.Logger

	runner    *build.Runner
	publisher *publish.Publisher

	repoDir    string // <state_dir>/repo, kept here so dedup checks don't have to reach through publisher
	stagingDir string // where successful build outputs are accumulated before publish

	// Webhook secret loaded once from disk; nil if no HTTP server.
	webhookSecret string

	// reloadCh is the buffered (size 1) signal channel used by Reload to
	// request a recipe-roster reload. The lifecycle goroutine inside
	// Run consumes from it. Coalesced — a Reload while one is pending
	// is dropped silently.
	reloadCh chan struct{}

	// Build failure tracking — keyed by recipeID+"@"+version. In-memory
	// only; daemon restart loses state and aggressively retries every
	// recipe (which is fine — restart is rare and failures are
	// well-logged).
	mu sync.Mutex
	// recipes is the current roster, mutated by Reload. The lifecycle
	// goroutine snapshots it under mu before constructing each watcher
	// epoch.
	recipes []recipe.Recipe
	// watcher points at the current epoch's watcher; tests inspect this.
	watcher         *watch.Watcher
	failures        map[string]failureRecord
	inFlight        *inFlightBuild
	buildsAttempted int
	buildsSucceeded int
}

// inFlightBuild describes the build the manager is currently working on,
// for status reporting. Set when a job enters the runner, cleared when
// the job completes (regardless of outcome).
type inFlightBuild struct {
	Recipe    string    `json:"recipe"`
	Version   string    `json:"version"`
	StartedAt time.Time `json:"started_at"`
}

// failureRecord tracks one (recipe, version) build that has failed at
// least once. Backoff is exponential: 1m, 5m, 15m, 1h, 6h, 24h, capped
// at 24h. The schedule keeps churn for transient failures (network, a
// flaky build) low and bounded, but allows persistent failures (a
// genuinely broken recipe) to retry once a day so a fix is picked up
// without needing operator intervention.
type failureRecord struct {
	failures  int
	nextRetry time.Time
}

// backoffSchedule defines how long to wait before retrying after the
// 1st, 2nd, 3rd, ... consecutive failure. Exhausted entries reuse the
// last (24h).
var backoffSchedule = []time.Duration{
	1 * time.Minute,
	5 * time.Minute,
	15 * time.Minute,
	1 * time.Hour,
	6 * time.Hour,
	24 * time.Hour,
}

func backoffFor(failures int) time.Duration {
	i := failures - 1
	if i < 0 {
		i = 0
	}
	if i >= len(backoffSchedule) {
		i = len(backoffSchedule) - 1
	}
	return backoffSchedule[i]
}

// Options collects construction-time inputs that aren't in the
// peipkg-config.toml — currently just the logger. Kept as a struct so
// the call site is named at point-of-use rather than positional.
type Options struct {
	Logger *slog.Logger
}

// New constructs a Manager from a parsed config plus options. It
// resolves the peipkg-build and peipkg-repo binaries on PATH, loads
// the recipe roster, and validates that everything required at runtime
// is present. It does NOT start any work.
func New(cfg config.Config, opts Options) (*Manager, error) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}

	peipkgBuild, err := exec.LookPath("peipkg-build")
	if err != nil {
		return nil, fmt.Errorf("peipkg-build not found on PATH: %w", err)
	}
	peipkgRepo, err := exec.LookPath("peipkg-repo")
	if err != nil {
		return nil, fmt.Errorf("peipkg-repo not found on PATH: %w", err)
	}

	recipes, err := recipe.LoadRoster(cfg.Manager.RecipesDir)
	if err != nil {
		return nil, fmt.Errorf("load recipe roster: %w", err)
	}
	logger.Info("recipe roster loaded", "count", len(recipes), "dir", cfg.Manager.RecipesDir)

	stateDirs := []string{
		cfg.Manager.StateDir,
		filepath.Join(cfg.Manager.StateDir, "sources"),
		filepath.Join(cfg.Manager.StateDir, "stage"),
		filepath.Join(cfg.Manager.StateDir, "publish"),
		filepath.Join(cfg.Manager.StateDir, "repo"),
	}
	for _, d := range stateDirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("create %s: %w", d, err)
		}
	}

	repoDir := filepath.Join(cfg.Manager.StateDir, "repo")
	runner := &build.Runner{
		PeipkgBuildPath: peipkgBuild,
		SourcesBaseDir:  filepath.Join(cfg.Manager.StateDir, "sources"),
		StageBaseDir:    filepath.Join(cfg.Manager.StateDir, "stage"),
	}
	publisher := &publish.Publisher{
		PeipkgRepoPath: peipkgRepo,
		RepoDir:        repoDir,
		SignKeyPath:    cfg.Signing.KeyFile,
		UploadBackend:  cfg.Upload.Backend,
		UploadRemote:   cfg.Upload.Remote,
	}

	var secret string
	if cfg.HTTP.Addr != "" && cfg.HTTP.WebhookSecretFile != "" {
		secret, err = watch.LoadSecret(cfg.HTTP.WebhookSecretFile)
		if err != nil {
			return nil, err
		}
	}

	return &Manager{
		cfg:           cfg,
		logger:        logger,
		recipes:       recipes,
		runner:        runner,
		publisher:     publisher,
		repoDir:       repoDir,
		stagingDir:    filepath.Join(cfg.Manager.StateDir, "publish"),
		webhookSecret: secret,
		reloadCh:      make(chan struct{}, 1),
	}, nil
}

// Run is the long-running daemon loop. It returns when ctx is
// cancelled, after waiting for the in-flight build (if any) to
// complete and shutting down the watcher.
//
// The watcher runs in epochs: each epoch is one Watcher.Run lifetime,
// scoped to an inner context derived from ctx. Reload cancels the
// current epoch's context and the lifecycle goroutine starts a fresh
// epoch with the freshly-loaded recipe roster. The HTTP server is
// inside the watcher, so it briefly bounces during a reload — this is
// a sub-second blip and is acceptable for the cron-driven recipe
// refresh that drives most reloads.
//
// In-flight builds use ctx (not the epoch context), so a reload does
// not interrupt them. They run to completion across the watcher
// transition.
func (m *Manager) Run(ctx context.Context) error {
	if err := m.ensureRepoInitialised(ctx); err != nil {
		return fmt.Errorf("initialise repo: %w", err)
	}

	// Single trigger channel reused across watcher epochs. Closed by
	// the lifecycle goroutine on outer-ctx cancel.
	triggers := make(chan watch.Trigger, 1024)

	var lifecycleWg sync.WaitGroup
	lifecycleWg.Add(1)
	go func() {
		defer lifecycleWg.Done()
		defer close(triggers)
		for {
			m.mu.Lock()
			recipes := append([]recipe.Recipe(nil), m.recipes...)
			m.mu.Unlock()

			epochCtx, epochCancel := context.WithCancel(ctx)
			w := &watch.Watcher{
				Recipes:       recipes,
				DefaultPoll:   m.cfg.Poll.DefaultInterval.Duration,
				WebhookSecret: m.webhookSecret,
				HTTPAddr:      m.cfg.HTTP.Addr,
				Logger:        m.logger,
				Triggers:      triggers,
				StatusHandler: m.statusHandler(),
			}
			m.mu.Lock()
			m.watcher = w
			m.mu.Unlock()

			watcherDone := make(chan struct{})
			go func() {
				defer close(watcherDone)
				if err := w.Run(epochCtx); err != nil {
					m.logger.Error("watcher exited", "err", err)
				}
			}()

			select {
			case <-ctx.Done():
				epochCancel()
				<-watcherDone
				return
			case <-m.reloadCh:
				m.logger.Info("reload requested; stopping watcher to swap recipe roster")
				epochCancel()
				<-watcherDone
				if err := m.loadRecipes(); err != nil {
					m.logger.Error("reload: load roster failed; continuing with existing recipes", "err", err)
					// m.recipes unchanged; the next epoch starts with
					// the pre-reload roster. Operator is expected to
					// fix the recipes_dir issue and reload again.
				}
			}
		}
	}()

	for trig := range triggers {
		if ctx.Err() != nil {
			break
		}
		m.handleTrigger(ctx, trig)
	}

	lifecycleWg.Wait()
	return ctx.Err()
}

// Reload requests that the daemon re-read its recipe roster from
// recipes_dir and replace the watcher's recipe set. Safe to call from
// a signal handler (non-blocking) and from any goroutine.
//
// Reloads are coalesced: if a reload is already pending, additional
// calls are no-ops. New recipes pick up on the next epoch; removed
// recipes' poll goroutines are cancelled and their ticker stops. The
// HTTP server bounces briefly across the transition.
//
// Reload does not interrupt in-flight builds — they use the outer Run
// context, which a reload does not cancel.
func (m *Manager) Reload() {
	if m.reloadCh == nil {
		return
	}
	select {
	case m.reloadCh <- struct{}{}:
	default:
		// already pending
	}
}

// triggerStillValid reports whether trig is still buildable under the
// CURRENT recipe roster. The watcher filters at emit time, but a
// recipe reload between emit and consume can invalidate a queued
// trigger — for example, a min_version bump introduced between the
// two events. The watcher's emit-time filter only sees triggers from
// its own epoch, not the queue carry-over from previous epochs.
//
// Returns false (drop trigger) if:
//   - The recipe is no longer in the roster (removed mid-flight).
//   - The recipe's current MinVersion forbids the captured version.
//
// Returns true (proceed) if neither rules out the trigger. Comparison
// errors fall through to true — recipe-load-time validation is the
// right place to reject malformed min_version values; tolerance here
// avoids silently dropping every trigger when an operator typo lands.
func (m *Manager) triggerStillValid(trig watch.Trigger) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	var current *recipe.Recipe
	for i := range m.recipes {
		if m.recipes[i].ID == trig.Recipe.ID {
			current = &m.recipes[i]
			break
		}
	}
	if current == nil {
		m.logger.Debug("trigger dropped (recipe removed from roster)",
			"recipe", trig.Recipe.ID, "tag", trig.UpstreamTag)
		return false
	}
	if current.Upstream.MinVersion == "" {
		return true
	}
	ok, err := pkgversion.UpstreamGTE(trig.Captured, current.Upstream.MinVersion)
	if err != nil {
		m.logger.Warn("min_version recheck failed; proceeding",
			"recipe", trig.Recipe.ID, "version", trig.Captured, "err", err)
		return true
	}
	if !ok {
		m.logger.Debug("trigger dropped (current recipe min_version forbids)",
			"recipe", trig.Recipe.ID, "version", trig.Captured,
			"min_version", current.Upstream.MinVersion)
		return false
	}
	return true
}

// loadRecipes re-reads recipes_dir into m.recipes. Called by the
// lifecycle goroutine inside Run after a reload signal.
func (m *Manager) loadRecipes() error {
	recipes, err := recipe.LoadRoster(m.cfg.Manager.RecipesDir)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.recipes = recipes
	m.mu.Unlock()
	m.logger.Info("recipes reloaded", "count", len(recipes), "dir", m.cfg.Manager.RecipesDir)
	return nil
}

// RunOnce performs a single sweep: poll every recipe once in parallel,
// process every emitted trigger, and exit. No webhook server, no
// ticker. Suitable for cron jobs, CI rebuilds, and manual operator
// "rebuild whatever's stale" runs.
//
// Returns when the trigger channel drains. Build/publish failures are
// logged but do not abort the sweep — the next invocation can retry.
func (m *Manager) RunOnce(ctx context.Context) error {
	if err := m.ensureRepoInitialised(ctx); err != nil {
		return fmt.Errorf("initialise repo: %w", err)
	}

	triggers := make(chan watch.Trigger, 1024)
	m.watcher = &watch.Watcher{
		Recipes:     m.recipes,
		DefaultPoll: m.cfg.Poll.DefaultInterval.Duration,
		Logger:      m.logger,
		Triggers:    triggers,
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := m.watcher.RunOnce(ctx); err != nil {
			m.logger.Error("watcher RunOnce", "err", err)
		}
		close(triggers)
	}()

	for trig := range triggers {
		if ctx.Err() != nil {
			break
		}
		m.handleTrigger(ctx, trig)
	}

	wg.Wait()
	return ctx.Err()
}

// ensureRepoInitialised creates a fresh repository state on first run.
// Idempotent — called every Run; subsequent invocations no-op.
func (m *Manager) ensureRepoInitialised(ctx context.Context) error {
	return m.publisher.Init(ctx, m.cfg.Repo.Name, m.cfg.Repo.Description, nowRFC3339())
}

// handleTrigger processes one watch trigger end-to-end: dedupe against
// the archive, check the failure-backoff schedule, build, publish on
// success.
//
// Build failures are logged AND recorded in the failure cache. The
// next poll cycle re-emits a trigger for the same tag, but the
// trigger is skipped until the backoff window elapses. This caps
// log churn for persistently-failing builds (one failure per backoff
// interval, not one per poll cycle) while still retrying on every
// schedule tick.
func (m *Manager) handleTrigger(ctx context.Context, trig watch.Trigger) {
	// Defence-in-depth: re-validate the trigger against the *current*
	// recipe roster before doing any work. The watcher filters on
	// emit, but reload semantics mean a stale trigger (emitted under
	// the previous epoch) might still be in the channel — we don't
	// want a recipe.MinVersion change to be defeated by a queued
	// trigger that predates the change. Same applies to a removed
	// recipe: the trigger is moot. Cheap check, runs once per build.
	if !m.triggerStillValid(trig) {
		return
	}

	version := composeVersion(trig.Captured, trig.Recipe.Upstream.PeiosRevision)

	already, err := m.alreadyPublished(trig.Recipe.ID, version)
	if err != nil {
		m.logger.Warn("dedup check failed; proceeding with build", "err", err)
	} else if already {
		m.logger.Debug("trigger skipped (already published)",
			"recipe", trig.Recipe.ID, "version", version, "source", trig.Source)
		return
	}

	if !m.shouldRetry(trig.Recipe.ID, version) {
		m.logger.Debug("trigger skipped (in failure backoff window)",
			"recipe", trig.Recipe.ID, "version", version, "source", trig.Source)
		return
	}

	m.logger.Info("starting build",
		"recipe", trig.Recipe.ID, "tag", trig.UpstreamTag,
		"version", version, "source", trig.Source)

	m.markInFlight(trig.Recipe.ID, version)
	defer m.clearInFlight()

	job := build.Job{
		Recipe:      trig.Recipe,
		UpstreamRef: trig.UpstreamTag,
		Version:     version,
		SourceRef:   composeSourceRef(trig.Recipe.Upstream.Git, trig.UpstreamTag),
		FarmID:      m.cfg.Manager.ID,
		SignKeyPath: m.cfg.Signing.KeyFile,
	}
	res, err := m.runner.Run(ctx, job)
	if err != nil {
		next := m.recordFailure(trig.Recipe.ID, version)
		m.logger.Error("build failed",
			"recipe", trig.Recipe.ID, "version", version,
			"err", err, "next_retry_after", next)
		return
	}

	// Move outputs from the per-build stage dir into the shared
	// publish-staging dir so peipkg-repo publish picks them up.
	if err := m.stageForPublish(res.Outputs); err != nil {
		m.logger.Error("stage outputs failed", "err", err)
		return
	}

	if err := m.publisher.Publish(ctx, m.stagingDir, nowRFC3339()); err != nil {
		next := m.recordFailure(trig.Recipe.ID, version)
		m.logger.Error("publish failed",
			"recipe", trig.Recipe.ID, "version", version,
			"err", err, "next_retry_after", next)
		// Outputs remain in stagingDir; next successful build will
		// publish them along with its own.
		return
	}

	// Build + publish succeeded — clear failure record (this version
	// is now considered a success) and clear stagingDir so the next
	// build doesn't re-publish the same files.
	m.clearFailure(trig.Recipe.ID, version)
	m.recordSuccess()
	if err := clearDir(m.stagingDir); err != nil {
		m.logger.Warn("clear staging dir failed", "err", err)
	}

	m.logger.Info("build + publish complete",
		"recipe", trig.Recipe.ID, "version", version,
		"outputs", len(res.Outputs), "build_timestamp", res.Timestamp)
}

// markInFlight records that the manager has started work on (recipe,
// version). Also bumps the attempted-builds counter.
func (m *Manager) markInFlight(recipeID, version string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inFlight = &inFlightBuild{
		Recipe:    recipeID,
		Version:   version,
		StartedAt: time.Now().UTC(),
	}
	m.buildsAttempted++
}

func (m *Manager) clearInFlight() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inFlight = nil
}

func (m *Manager) recordSuccess() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.buildsSucceeded++
}

// shouldRetry reports whether a (recipe, version) build that previously
// failed is now eligible for a retry. Returns true for the first attempt
// (no failure record yet) or after the backoff window has elapsed.
func (m *Manager) shouldRetry(recipeID, version string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.failures[failureKey(recipeID, version)]
	if !ok {
		return true
	}
	return time.Now().After(rec.nextRetry)
}

// recordFailure increments the failure count for (recipe, version) and
// computes the next retry time. Returns the duration until that retry,
// for log surfacing.
func (m *Manager) recordFailure(recipeID, version string) time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failures == nil {
		m.failures = make(map[string]failureRecord)
	}
	k := failureKey(recipeID, version)
	rec := m.failures[k]
	rec.failures++
	d := backoffFor(rec.failures)
	rec.nextRetry = time.Now().Add(d)
	m.failures[k] = rec
	return d
}

// clearFailure removes the failure record for (recipe, version). Called
// after a successful build+publish.
func (m *Manager) clearFailure(recipeID, version string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.failures, failureKey(recipeID, version))
}

func failureKey(recipeID, version string) string {
	return recipeID + "@" + version
}

// Status is the JSON payload returned by the /status endpoint.
type Status struct {
	FarmID          string                   `json:"farm_id"`
	Recipes         []string                 `json:"recipes"`
	BuildsAttempted int                      `json:"builds_attempted"`
	BuildsSucceeded int                      `json:"builds_succeeded"`
	InFlight        *inFlightBuild           `json:"in_flight,omitempty"`
	Failures        map[string]failureReport `json:"failures,omitempty"`
}

// failureReport is the JSON-friendly view of a failureRecord.
type failureReport struct {
	Failures  int       `json:"failures"`
	NextRetry time.Time `json:"next_retry"`
}

// snapshot returns the current Status under the manager's lock. Used
// by the HTTP handler and by tests.
func (m *Manager) snapshot() Status {
	m.mu.Lock()
	defer m.mu.Unlock()

	recipes := make([]string, len(m.recipes))
	for i, r := range m.recipes {
		recipes[i] = r.ID
	}

	var inFlight *inFlightBuild
	if m.inFlight != nil {
		copy := *m.inFlight
		inFlight = &copy
	}

	var failures map[string]failureReport
	if len(m.failures) > 0 {
		failures = make(map[string]failureReport, len(m.failures))
		for k, v := range m.failures {
			failures[k] = failureReport{
				Failures:  v.failures,
				NextRetry: v.nextRetry.UTC(),
			}
		}
	}

	return Status{
		FarmID:          m.cfg.Manager.ID,
		Recipes:         recipes,
		BuildsAttempted: m.buildsAttempted,
		BuildsSucceeded: m.buildsSucceeded,
		InFlight:        inFlight,
		Failures:        failures,
	}
}

// statusHandler returns an http.Handler that serves the manager's
// current state as JSON.
func (m *Manager) statusHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(m.snapshot()); err != nil {
			http.Error(w, "encode status", http.StatusInternalServerError)
			return
		}
	})
}

// stageForPublish moves the per-build outputs into the shared publish
// staging directory. Atomic rename when on the same filesystem (the
// usual case under one state_dir).
func (m *Manager) stageForPublish(outputs []string) error {
	if err := os.MkdirAll(m.stagingDir, 0o755); err != nil {
		return err
	}
	for _, src := range outputs {
		dst := filepath.Join(m.stagingDir, filepath.Base(src))
		if err := os.Rename(src, dst); err != nil {
			return fmt.Errorf("move %s → %s: %w", src, dst, err)
		}
	}
	return nil
}

// archivePackageEntry is the subset of peipkg-repo's archive.json
// per-package entry that the manager needs for dedup.
type archivePackageEntry struct {
	Name         string `json:"name"`
	Version      string `json:"version"`
	Architecture string `json:"architecture"`
}

// archiveSnapshot is the subset of archive.json the manager parses.
type archiveSnapshot struct {
	Packages []archivePackageEntry `json:"packages"`
}

// alreadyPublished reports whether the repository's archive index
// already contains an entry for (name, version) at any architecture.
//
// Architecture-aware dedup would matter for multi-arch repos, but in
// v0 a recipe builds for one architecture and the archive sees
// (name, version, arch) tuples that are uniquely keyed. We dedupe on
// (name, version) for simplicity; re-published archive entries are
// rejected by peipkg-repo's retention rule even if we do somehow
// trigger a duplicate.
func (m *Manager) alreadyPublished(recipeID, version string) (bool, error) {
	archivePath := filepath.Join(m.repoDir, "index", "archive.json")
	data, err := os.ReadFile(archivePath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil // empty repo
		}
		return false, fmt.Errorf("read archive: %w", err)
	}
	var snap archiveSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return false, fmt.Errorf("parse archive: %w", err)
	}
	for _, p := range snap.Packages {
		if p.Name == recipeID && p.Version == version {
			return true, nil
		}
	}
	return false, nil
}

// composeVersion builds a peipkg version string from a regex-captured
// upstream version and a recipe-supplied peios revision.
//
// Examples:
//
//	composeVersion("1.3.2", 1)  // → "1.3.2-1"
//	composeVersion("1.0.0-rc.1", 2) // → "1.0.0-rc.1-2"
func composeVersion(captured string, peiosRevision int) string {
	if peiosRevision < 1 {
		peiosRevision = 1
	}
	return fmt.Sprintf("%s-%d", captured, peiosRevision)
}

// composeSourceRef builds the manifest's build.source_ref value from
// the upstream git URL and tag. Format matches PSD-009 §3.3.4's
// recommended `git+<url>#<ref>` form.
func composeSourceRef(gitURL, tag string) string {
	return fmt.Sprintf("git+%s#refs/tags/%s", gitURL, tag)
}

// nowRFC3339 returns the current UTC time formatted to satisfy
// PSD-009 §3.3.4's "ends with Z" requirement.
//
// v0 uses the current wall-clock time per build. A future improvement
// is to derive the timestamp from the upstream tag's commit timestamp
// for true reproducibility — but that requires a `git show -s` per
// build, and the inputs would still need pinning, so it's deferred.
func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// clearDir removes every entry directly under dir, leaving dir itself
// in place.
func clearDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}
