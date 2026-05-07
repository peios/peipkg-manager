package recipe

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func makeRecipeDir(t *testing.T, root, name, body string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "peipkg.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const fullRecipe = `
[meta]
license = "MIT"
build_script = "build.sh"

[[package]]
name = "libfoo"
architecture = "x86_64"

[upstream]
git = "https://github.com/example/libfoo"
tag_pattern = "^v(\\d+\\.\\d+\\.\\d+)$"
peios_revision = 1

[watch]
github_webhook = true
poll_interval = "30m"
`

const upstreamlessRecipe = `
[meta]
license = "MIT"
build_script = "build.sh"

[[package]]
name = "manualonly"
architecture = "noarch"
# no [upstream], no [watch] — manual builds only
`

func TestLoadRosterFindsAndSorts(t *testing.T) {
	root := t.TempDir()
	makeRecipeDir(t, root, "libfoo", fullRecipe)
	makeRecipeDir(t, root, "alpha", fullRecipe)
	makeRecipeDir(t, root, "manualonly", upstreamlessRecipe)

	// Stray subdir without a peipkg.toml — should be silently skipped.
	if err := os.MkdirAll(filepath.Join(root, "scratch"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := LoadRoster(root)
	if err != nil {
		t.Fatal(err)
	}
	wantIDs := []string{"alpha", "libfoo", "manualonly"}
	if len(got) != len(wantIDs) {
		t.Fatalf("got %d recipes, want %d (%v)", len(got), len(wantIDs), got)
	}
	for i, id := range wantIDs {
		if got[i].ID != id {
			t.Errorf("got[%d].ID = %q, want %q", i, got[i].ID, id)
		}
	}
}

func TestLoadRecipeReadsUpstreamAndWatch(t *testing.T) {
	root := t.TempDir()
	makeRecipeDir(t, root, "libfoo", fullRecipe)

	got, err := LoadRoster(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d recipes", len(got))
	}
	r := got[0]
	if !r.Upstream.HasUpstream() {
		t.Error("HasUpstream = false, want true")
	}
	if r.Upstream.Git != "https://github.com/example/libfoo" {
		t.Errorf("Upstream.Git = %q", r.Upstream.Git)
	}
	if r.Upstream.PeiosRevision != 1 {
		t.Errorf("Upstream.PeiosRevision = %d", r.Upstream.PeiosRevision)
	}
	if !r.Watch.GitHubWebhook {
		t.Error("Watch.GitHubWebhook = false, want true")
	}
	if r.Watch.PollInterval == nil {
		t.Fatal("Watch.PollInterval is nil")
	}
	if r.Watch.PollInterval.Duration != 30*time.Minute {
		t.Errorf("Watch.PollInterval = %v, want 30m", r.Watch.PollInterval.Duration)
	}
}

func TestLoadRecipeUpstreamlessWorks(t *testing.T) {
	root := t.TempDir()
	makeRecipeDir(t, root, "manualonly", upstreamlessRecipe)

	got, err := LoadRoster(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d recipes", len(got))
	}
	r := got[0]
	if r.Upstream.HasUpstream() {
		t.Error("HasUpstream = true on a recipe with no [upstream]")
	}
	// The recipe loads cleanly even without manager-relevant sections;
	// peipkg-build can still be invoked against it on manual trigger.
}

func TestLoadRecipeIgnoresPeipkgBuildSections(t *testing.T) {
	// Exercise the multi-tool tolerance: the recipe has [meta] and
	// [[package]] (peipkg-build's territory). We must NOT error out
	// because we don't decode them.
	body := `
[meta]
license = "MIT"
build_script = "build.sh"

[[package]]
name = "x"
architecture = "noarch"

[upstream]
git = "https://example.com/x"
tag_pattern = "^v(.+)$"

[watch]
poll_interval = "1h"
`
	root := t.TempDir()
	makeRecipeDir(t, root, "x", body)
	got, err := LoadRoster(root)
	if err != nil {
		t.Fatalf("LoadRoster failed unexpectedly: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d recipes", len(got))
	}
}
