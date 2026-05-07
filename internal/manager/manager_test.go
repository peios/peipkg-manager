package manager

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peios/peipkg-manager/internal/config"
	"github.com/peios/peipkg-manager/internal/recipe"
)

func TestComposeVersion(t *testing.T) {
	cases := []struct {
		captured string
		rev      int
		want     string
	}{
		{"1.3.2", 1, "1.3.2-1"},
		{"1.0.0-rc.1", 2, "1.0.0-rc.1-2"},
		{"0.5", 0, "0.5-1"}, // rev < 1 clamps to 1
	}
	for _, c := range cases {
		got := composeVersion(c.captured, c.rev)
		if got != c.want {
			t.Errorf("composeVersion(%q, %d) = %q, want %q", c.captured, c.rev, got, c.want)
		}
	}
}

func TestComposeSourceRef(t *testing.T) {
	got := composeSourceRef("https://github.com/example/foo", "v1.0.0")
	want := "git+https://github.com/example/foo#refs/tags/v1.0.0"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBackoffSchedule(t *testing.T) {
	cases := []struct {
		failures int
		want     time.Duration
	}{
		{0, 1 * time.Minute}, // negative becomes index 0 → 1m (defensive)
		{1, 1 * time.Minute},
		{2, 5 * time.Minute},
		{3, 15 * time.Minute},
		{4, 1 * time.Hour},
		{5, 6 * time.Hour},
		{6, 24 * time.Hour},
		{7, 24 * time.Hour}, // capped
		{99, 24 * time.Hour},
	}
	for _, c := range cases {
		got := backoffFor(c.failures)
		if got != c.want {
			t.Errorf("backoffFor(%d) = %v, want %v", c.failures, got, c.want)
		}
	}
}

func TestStatusSnapshotAndHandler(t *testing.T) {
	mgr := &Manager{
		cfg: config.Config{
			Manager: config.Manager{ID: "test-farm"},
		},
		recipes: []recipe.Recipe{
			{ID: "alpha"},
			{ID: "libfoo"},
		},
	}

	// Empty snapshot.
	s := mgr.snapshot()
	if s.FarmID != "test-farm" {
		t.Errorf("FarmID = %q", s.FarmID)
	}
	if len(s.Recipes) != 2 || s.Recipes[0] != "alpha" || s.Recipes[1] != "libfoo" {
		t.Errorf("Recipes = %v", s.Recipes)
	}
	if s.InFlight != nil {
		t.Errorf("InFlight should be nil at startup, got %+v", s.InFlight)
	}
	if s.BuildsAttempted != 0 || s.BuildsSucceeded != 0 {
		t.Errorf("counters should be 0, got attempted=%d succeeded=%d", s.BuildsAttempted, s.BuildsSucceeded)
	}

	// Mid-build snapshot.
	mgr.markInFlight("libfoo", "1.2.3-1")
	s = mgr.snapshot()
	if s.InFlight == nil {
		t.Fatal("InFlight should be set after markInFlight")
	}
	if s.InFlight.Recipe != "libfoo" || s.InFlight.Version != "1.2.3-1" {
		t.Errorf("InFlight = %+v", s.InFlight)
	}
	if s.BuildsAttempted != 1 {
		t.Errorf("BuildsAttempted = %d, want 1", s.BuildsAttempted)
	}

	mgr.recordSuccess()
	mgr.clearInFlight()
	s = mgr.snapshot()
	if s.BuildsSucceeded != 1 {
		t.Errorf("BuildsSucceeded = %d, want 1", s.BuildsSucceeded)
	}
	if s.InFlight != nil {
		t.Errorf("InFlight should be nil after clearInFlight, got %+v", s.InFlight)
	}

	// Failure surfaces in the snapshot.
	mgr.recordFailure("alpha", "0.5-1")
	s = mgr.snapshot()
	if len(s.Failures) != 1 {
		t.Fatalf("Failures count = %d", len(s.Failures))
	}
	rep, ok := s.Failures["alpha@0.5-1"]
	if !ok {
		t.Fatalf("missing failure key, got: %v", s.Failures)
	}
	if rep.Failures != 1 {
		t.Errorf("failure count = %d, want 1", rep.Failures)
	}

	// HTTP handler renders the snapshot as JSON.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/status", nil)
	mgr.statusHandler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("HTTP status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}
	var got Status
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body: %v\n%s", err, rec.Body.String())
	}
	if got.FarmID != "test-farm" {
		t.Errorf("decoded FarmID = %q", got.FarmID)
	}
}

func TestFailureLifecycle(t *testing.T) {
	mgr := &Manager{}

	// Fresh state: every (recipe, version) is eligible.
	if !mgr.shouldRetry("foo", "1.0-1") {
		t.Error("fresh recipe should be eligible to retry")
	}

	// After a failure: should be in backoff for a minute.
	d := mgr.recordFailure("foo", "1.0-1")
	if d != 1*time.Minute {
		t.Errorf("first-failure backoff = %v, want 1m", d)
	}
	if mgr.shouldRetry("foo", "1.0-1") {
		t.Error("just-failed recipe should be in backoff window")
	}

	// Different (recipe, version) should be unaffected.
	if !mgr.shouldRetry("foo", "1.0-2") {
		t.Error("different version should not inherit failure state")
	}
	if !mgr.shouldRetry("bar", "1.0-1") {
		t.Error("different recipe should not inherit failure state")
	}

	// Second failure escalates the backoff.
	d2 := mgr.recordFailure("foo", "1.0-1")
	if d2 != 5*time.Minute {
		t.Errorf("second-failure backoff = %v, want 5m", d2)
	}

	// Clear: back to fresh.
	mgr.clearFailure("foo", "1.0-1")
	if !mgr.shouldRetry("foo", "1.0-1") {
		t.Error("cleared recipe should be eligible again")
	}
}

func TestAlreadyPublishedReadsArchive(t *testing.T) {
	dir := t.TempDir()
	repoDir := filepath.Join(dir, "repo")
	indexDir := filepath.Join(repoDir, "index")
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		t.Fatal(err)
	}
	archive := map[string]any{
		"schema_version": 1,
		"kind":           "archive",
		"packages": []map[string]any{
			{"name": "libfoo", "version": "1.0.0-1", "architecture": "x86_64"},
			{"name": "libfoo", "version": "1.0.1-1", "architecture": "x86_64"},
		},
	}
	body, _ := json.Marshal(archive)
	if err := os.WriteFile(filepath.Join(indexDir, "archive.json"), body, 0o644); err != nil {
		t.Fatal(err)
	}

	mgr := &Manager{repoDir: repoDir}

	cases := []struct {
		name, ver string
		want      bool
	}{
		{"libfoo", "1.0.0-1", true},
		{"libfoo", "1.0.1-1", true},
		{"libfoo", "2.0.0-1", false}, // not in archive
		{"libbar", "1.0.0-1", false}, // wrong name
	}
	for _, c := range cases {
		got, err := mgr.alreadyPublished(c.name, c.ver)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("alreadyPublished(%s, %s) = %v, want %v", c.name, c.ver, got, c.want)
		}
	}
}

func TestReloadCoalesces(t *testing.T) {
	mgr := &Manager{reloadCh: make(chan struct{}, 1)}

	// Two consecutive Reloads with no consumer in between should
	// coalesce: only one signal pending.
	mgr.Reload()
	mgr.Reload()

	select {
	case <-mgr.reloadCh:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected one reload signal pending after Reload()")
	}
	select {
	case <-mgr.reloadCh:
		t.Fatal("second Reload should have been dropped (channel coalesces)")
	default:
	}
}

func TestReloadIsNoOpBeforeRun(t *testing.T) {
	// A Manager constructed with reloadCh nil (e.g., in a test stub)
	// must not panic when Reload is called.
	mgr := &Manager{}
	mgr.Reload() // should not panic
}

func TestLoadRecipesPicksUpAddedRecipe(t *testing.T) {
	dir := t.TempDir()

	// Initial state: one recipe.
	mustMkRecipe(t, filepath.Join(dir, "alpha"))

	mgr := &Manager{
		cfg:    config.Config{Manager: config.Manager{RecipesDir: dir}},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := mgr.loadRecipes(); err != nil {
		t.Fatalf("first load: %v", err)
	}
	if len(mgr.recipes) != 1 || mgr.recipes[0].ID != "alpha" {
		t.Fatalf("after first load: recipes = %v", mgr.recipes)
	}

	// Add a second recipe and reload.
	mustMkRecipe(t, filepath.Join(dir, "beta"))
	if err := mgr.loadRecipes(); err != nil {
		t.Fatalf("second load: %v", err)
	}
	if len(mgr.recipes) != 2 {
		t.Fatalf("after second load: expected 2 recipes, got %d (%v)", len(mgr.recipes), mgr.recipes)
	}
	got := []string{mgr.recipes[0].ID, mgr.recipes[1].ID}
	if got[0] != "alpha" || got[1] != "beta" {
		t.Errorf("expected sorted [alpha beta], got %v", got)
	}

	// Remove one and reload.
	if err := os.RemoveAll(filepath.Join(dir, "alpha")); err != nil {
		t.Fatal(err)
	}
	if err := mgr.loadRecipes(); err != nil {
		t.Fatalf("third load: %v", err)
	}
	if len(mgr.recipes) != 1 || mgr.recipes[0].ID != "beta" {
		t.Errorf("after removal: recipes = %v, want [beta]", mgr.recipes)
	}
}

func TestLoadRecipesReturnsErrorOnMissingDir(t *testing.T) {
	mgr := &Manager{
		cfg:    config.Config{Manager: config.Manager{RecipesDir: "/nonexistent/recipes"}},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := mgr.loadRecipes(); err == nil {
		t.Fatal("expected error for missing recipes_dir, got nil")
	}
}

// mustMkRecipe creates an empty-but-valid recipe directory. Used by
// reload tests where build correctness doesn't matter — we only care
// that the loader sees the recipe.
func mustMkRecipe(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `
[meta]
license = "MIT"
homepage = "https://example.com"
build_script = "build.sh"

[[package]]
name = "x"
architecture = "noarch"
description = "test"
files = []
`
	if err := os.WriteFile(filepath.Join(dir, "peipkg.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAlreadyPublishedReturnsFalseOnEmptyRepo(t *testing.T) {
	mgr := &Manager{repoDir: t.TempDir()}
	got, err := mgr.alreadyPublished("anything", "1.0-1")
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Error("alreadyPublished returned true for an empty repo")
	}
}

// TestEndToEndAgainstLocalUpstream runs the full pipeline against:
//
//   - a local git repository as the upstream (two version tags)
//   - a recipe that just copies files into DESTDIR (no real build)
//   - peipkg-build and peipkg-repo binaries resolved from PATH
//
// The test exercises every layer: poll, dedup, build orchestration,
// publish, archive accumulation. Skipped if the peipkg-build /
// peipkg-repo binaries are not on PATH (CI installs them).
func TestEndToEndAgainstLocalUpstream(t *testing.T) {
	for _, bin := range []string{"git", "peipkg-build", "peipkg-repo"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("required binary %q not on PATH; skipping E2E", bin)
		}
	}

	dir := t.TempDir()

	// 1. Local git "upstream" with two version tags.
	upstreamDir := filepath.Join(dir, "upstream")
	makeUpstreamRepo(t, upstreamDir, []string{"v1.0.0", "v1.0.1"})

	// 2. A peipkg-build recipe: minimal noop build.
	recipesDir := filepath.Join(dir, "recipes")
	makeRecipe(t, filepath.Join(recipesDir, "hello"), upstreamDir)

	// 3. Signing key.
	keyPath := filepath.Join(dir, "signing.ed25519")
	makeSigningKey(t, keyPath)

	// 4. peipkg-config.toml.
	cfgPath := filepath.Join(dir, "config.toml")
	stateDir := filepath.Join(dir, "state")
	cfgBody := fmt.Sprintf(`
[manager]
id = "test-farm"
recipes_dir = %q
state_dir = %q

[repo]
name = "test-repo"
description = "E2E test"

[signing]
key_file = %q

[upload]
backend = "none"

[poll]
default_interval = "1m"
`, recipesDir, stateDir, keyPath)
	if err := os.WriteFile(cfgPath, []byte(cfgBody), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	mgr, err := New(cfg, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// RunOnce: poll all recipes once, process triggers, exit. No
	// timing dance with cancellation — RunOnce returns when the work
	// is done, so the test waits exactly as long as the actual build
	// pipeline takes.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := mgr.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	activePath := filepath.Join(stateDir, "repo", "index", "active.json")
	data, err := os.ReadFile(activePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"name":"hello"`) {
		t.Fatalf("active.json missing hello entry:\n%s", data)
	}

	// Inspect the archive: both versions should have built.
	archivePath := filepath.Join(stateDir, "repo", "index", "archive.json")
	body, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	var archive struct {
		Packages []struct {
			Name, Version string
		} `json:"packages"`
	}
	if err := json.Unmarshal(body, &archive); err != nil {
		t.Fatal(err)
	}
	gotVersions := map[string]bool{}
	for _, p := range archive.Packages {
		if p.Name == "hello" {
			gotVersions[p.Version] = true
		}
	}
	for _, v := range []string{"1.0.0-1", "1.0.1-1"} {
		if !gotVersions[v] {
			t.Errorf("archive missing hello version %q (got: %v)", v, gotVersions)
		}
	}
}

// makeUpstreamRepo builds a tiny git repo that looks plausible to
// peipkg-build's build.sh: just a file the build.sh copies.
func makeUpstreamRepo(t *testing.T, dir string, tags []string) {
	t.Helper()
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
		// One file in a path the recipe's [[package]].files list will claim.
		messagePath := filepath.Join(dir, "usr", "share", "hello")
		if err := os.MkdirAll(messagePath, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(messagePath, "MESSAGE"), []byte("v"+tag), 0o644); err != nil {
			t.Fatal(err)
		}
		run("add", ".")
		run("commit", "-q", "-m", "v"+tag)
		run("tag", tag)
	}
}

func makeRecipe(t *testing.T, dir, upstreamGit string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	recipeBody := fmt.Sprintf(`
[meta]
license = "MIT"
homepage = "https://example.com/hello"
build_script = "build.sh"

[[package]]
name = "hello"
architecture = "noarch"
description = "test fixture"
files = [ "usr/share/hello/MESSAGE" ]

[upstream]
git = %q
tag_pattern = "^v(\\d+\\.\\d+\\.\\d+)$"
peios_revision = 1

[watch]
poll_interval = "1m"
`, upstreamGit)
	if err := os.WriteFile(filepath.Join(dir, "peipkg.toml"), []byte(recipeBody), 0o644); err != nil {
		t.Fatal(err)
	}
	build := `#!/bin/sh
set -eu
cp -a "$SOURCE_DIR/." "$DESTDIR/"
`
	if err := os.WriteFile(filepath.Join(dir, "build.sh"), []byte(build), 0o755); err != nil {
		t.Fatal(err)
	}
}

func makeSigningKey(t *testing.T, path string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
}
