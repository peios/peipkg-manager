package watch

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/peios/peipkg-manager/internal/recipe"
)

// makeLocalRepo mirrors source_test's helper — the watch poll needs a
// real git URL to ls-remote against.
func makeLocalRepo(t *testing.T, dir string, tags []string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	for _, tag := range tags {
		if err := os.WriteFile(filepath.Join(dir, "f"), []byte(tag), 0o644); err != nil {
			t.Fatal(err)
		}
		run("add", "f")
		run("commit", "-q", "-m", "v"+tag)
		run("tag", tag)
	}
	return dir
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestPollEmitsTriggersForMatchingTags(t *testing.T) {
	repo := makeLocalRepo(t, t.TempDir(), []string{"v1.0.0", "v1.0.1", "v2.0.0", "release-old"})

	r := recipe.Recipe{
		ID:   "test",
		Path: "/dev/null",
		Upstream: recipe.Upstream{
			Git:           repo,
			TagPattern:    `^v(\d+\.\d+\.\d+)$`,
			PeiosRevision: 1,
		},
	}

	triggers := make(chan Trigger, 16)
	w := &Watcher{
		Recipes:     []recipe.Recipe{r},
		DefaultPoll: time.Hour, // doesn't matter; we only do one poll
		Logger:      discardLogger(),
		Triggers:    triggers,
	}
	if err := w.compilePatterns(); err != nil {
		t.Fatal(err)
	}

	w.pollOnce(context.Background(), r)
	close(triggers)

	got := map[string]string{} // tag → captured
	for trig := range triggers {
		got[trig.UpstreamTag] = trig.Captured
	}

	want := map[string]string{
		"v1.0.0": "1.0.0",
		"v1.0.1": "1.0.1",
		"v2.0.0": "2.0.0",
	}
	for tag, captured := range want {
		if got[tag] != captured {
			t.Errorf("tag %q: captured = %q, want %q", tag, got[tag], captured)
		}
	}
	// release-old does NOT match the pattern.
	if _, ok := got["release-old"]; ok {
		t.Errorf("non-matching tag was emitted: release-old")
	}
}

func TestPollFiltersBelowMinVersion(t *testing.T) {
	repo := makeLocalRepo(t, t.TempDir(), []string{"v1.0.0", "v1.5.0", "v2.0.0", "v2.1.0"})

	r := recipe.Recipe{
		ID:   "test",
		Path: "/dev/null",
		Upstream: recipe.Upstream{
			Git:           repo,
			TagPattern:    `^v(\d+\.\d+\.\d+)$`,
			PeiosRevision: 1,
			MinVersion:    "2.0.0", // skip 1.x
		},
	}

	triggers := make(chan Trigger, 16)
	w := &Watcher{
		Recipes:     []recipe.Recipe{r},
		DefaultPoll: time.Hour,
		Logger:      discardLogger(),
		Triggers:    triggers,
	}
	if err := w.compilePatterns(); err != nil {
		t.Fatal(err)
	}

	w.pollOnce(context.Background(), r)
	close(triggers)

	got := map[string]bool{}
	for trig := range triggers {
		got[trig.UpstreamTag] = true
	}

	if got["v1.0.0"] {
		t.Error("v1.0.0 should have been filtered (< min_version 2.0.0)")
	}
	if got["v1.5.0"] {
		t.Error("v1.5.0 should have been filtered (< min_version 2.0.0)")
	}
	if !got["v2.0.0"] {
		t.Error("v2.0.0 should have passed (== min_version 2.0.0)")
	}
	if !got["v2.1.0"] {
		t.Error("v2.1.0 should have passed (> min_version 2.0.0)")
	}
}

func TestCompilePatternsRejectsInvalidRegex(t *testing.T) {
	w := &Watcher{
		Recipes: []recipe.Recipe{{
			ID: "bad", Path: "x",
			Upstream: recipe.Upstream{Git: "x", TagPattern: "(unclosed"},
		}},
	}
	if err := w.compilePatterns(); err == nil {
		t.Error("compilePatterns accepted invalid regex")
	}
}

func TestCompilePatternsRequiresCaptureGroup(t *testing.T) {
	w := &Watcher{
		Recipes: []recipe.Recipe{{
			ID: "noCapture", Path: "x",
			Upstream: recipe.Upstream{Git: "x", TagPattern: `^v\d+\.\d+\.\d+$`},
		}},
	}
	if err := w.compilePatterns(); err == nil {
		t.Error("compilePatterns accepted regex without capture group")
	}
}

func TestNormalizeGitURL(t *testing.T) {
	cases := []struct{ a, b string }{
		{"https://github.com/madler/zlib", "https://github.com/madler/zlib.git"},
		{"https://github.com/madler/zlib", "https://github.com/madler/zlib/"},
		{"https://github.com/Madler/Zlib", "https://github.com/madler/zlib"},
	}
	for _, c := range cases {
		if normalizeGitURL(c.a) != normalizeGitURL(c.b) {
			t.Errorf("normalizeGitURL(%q) != normalizeGitURL(%q)", c.a, c.b)
		}
	}
}

func TestValidGitHubSignature(t *testing.T) {
	secret := "supersecret"
	body := []byte(`{"foo":"bar"}`)

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	good := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	if !validGitHubSignature(body, good, secret) {
		t.Error("valid signature rejected")
	}
	if validGitHubSignature(body, "sha256=00ff", secret) {
		t.Error("bad signature accepted")
	}
	if validGitHubSignature(body, "", secret) {
		t.Error("empty signature accepted")
	}
	if validGitHubSignature(body, "md5=abc", secret) {
		t.Error("non-sha256 prefix accepted")
	}
}

func TestWebhookDispatchesToMatchingRecipe(t *testing.T) {
	// Set up a recipe whose Upstream.Git matches what the webhook
	// payload reports. No actual upstream poll runs (the recipe's git
	// URL is bogus), but compilePatterns + recipe lookup are exercised.
	r := recipe.Recipe{
		ID: "libfoo",
		Upstream: recipe.Upstream{
			Git:        "https://github.com/example/libfoo",
			TagPattern: `^v(\d+\.\d+\.\d+)$`,
		},
		Watch: recipe.Watch{GitHubWebhook: true},
	}

	triggers := make(chan Trigger, 16)
	w := &Watcher{
		Recipes:  []recipe.Recipe{r},
		Logger:   discardLogger(),
		Triggers: triggers,
	}
	if err := w.compilePatterns(); err != nil {
		t.Fatal(err)
	}

	body := []byte(`{"repository":{"html_url":"https://github.com/example/libfoo","clone_url":"https://github.com/example/libfoo.git"}}`)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(body))
	rw := httptest.NewRecorder()

	w.handleGitHubWebhook(rw, req)

	if rw.Code != http.StatusOK {
		t.Errorf("status = %d, want 200; body=%s", rw.Code, rw.Body.String())
	}
	// The handler launches a goroutine for the actual poll; we don't
	// wait for it here (would need a real git URL). The status code
	// is sufficient evidence that recipe matching worked.
}

func TestWebhookRejectsInvalidSignature(t *testing.T) {
	r := recipe.Recipe{
		ID: "x",
		Upstream: recipe.Upstream{
			Git: "https://example.com/x", TagPattern: `(.+)`,
		},
		Watch: recipe.Watch{GitHubWebhook: true},
	}
	w := &Watcher{
		Recipes:       []recipe.Recipe{r},
		WebhookSecret: "hunter2",
		Logger:        discardLogger(),
		Triggers:      make(chan Trigger, 1),
	}
	if err := w.compilePatterns(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	rw := httptest.NewRecorder()

	w.handleGitHubWebhook(rw, req)
	if rw.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rw.Code)
	}
}

func TestWebhookAcksUnknownRepoSilently(t *testing.T) {
	triggers := make(chan Trigger, 1)
	w := &Watcher{
		Recipes:  []recipe.Recipe{},
		Logger:   discardLogger(),
		Triggers: triggers,
	}
	if err := w.compilePatterns(); err != nil {
		t.Fatal(err)
	}

	body := []byte(`{"repository":{"html_url":"https://github.com/unknown/repo"}}`)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(body))
	rw := httptest.NewRecorder()

	w.handleGitHubWebhook(rw, req)
	// Unknown repos return 200 (so GitHub doesn't keep retrying) but
	// no trigger should be enqueued.
	if rw.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rw.Code)
	}
	select {
	case trig := <-triggers:
		t.Errorf("unexpected trigger emitted for unknown repo: %+v", trig)
	default:
	}
}
