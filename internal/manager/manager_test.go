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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peios/peipkg-manager/internal/config"
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

	// Run for a few seconds — long enough for poll → dedup → build →
	// publish to complete, short enough not to drag out the test.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- mgr.Run(ctx) }()

	// Poll the active index until it contains a hello entry, or fail
	// after a timeout. The poll → build → publish cycle is roughly
	// "ls-remote + 1 git clone + peipkg-build + peipkg-repo publish",
	// which is well under 10 seconds on typical hardware.
	deadline := time.Now().Add(20 * time.Second)
	activePath := filepath.Join(stateDir, "repo", "index", "active.json")
	for {
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatalf("active.json never contained a hello entry within timeout")
		}
		data, err := os.ReadFile(activePath)
		if err == nil && strings.Contains(string(data), `"name":"hello"`) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	cancel()
	if err := <-done; err != nil && err != context.Canceled {
		t.Errorf("manager.Run returned non-cancel error: %v", err)
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
