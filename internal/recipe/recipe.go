// Package recipe scans a recipes directory and loads the
// manager-relevant subset of each peipkg.toml.
//
// peipkg-build owns [meta] and [[package]] sections of a recipe; this
// package owns [upstream] (where source lives, how to recognise version
// tags) and [watch] (how the manager learns about new versions). The
// recipe file format is documented in the PSD-009 appendix on recipe
// formats.
//
// Recipes whose peipkg.toml lacks an [upstream] section are loaded
// (they're still valid build inputs) but not auto-watched: an operator
// must trigger their builds manually. Recipes without a [watch] section
// are watched on the polling default only.
package recipe

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/BurntSushi/toml"

	"github.com/peios/peipkg-manager/internal/config"
)

// Recipe is one recipe's manager-relevant view. ID is the recipe's
// directory name (e.g., "libfoo" for recipes/libfoo/peipkg.toml); Path
// is the absolute path to the peipkg.toml file (the same path
// peipkg-build is invoked against).
type Recipe struct {
	ID       string
	Dir      string // directory containing peipkg.toml + build.sh
	Path     string // absolute path to peipkg.toml
	Upstream Upstream
	Watch    Watch
}

// Upstream describes where source lives and which tags we care about.
//
// A recipe with empty Upstream.Git is loaded (peipkg-build can still
// run against it on manual trigger) but cannot be auto-built — the
// manager has no way to discover new versions.
type Upstream struct {
	Git           string `toml:"git"`
	TagPattern    string `toml:"tag_pattern"`
	PeiosRevision int    `toml:"peios_revision"`
}

// HasUpstream reports whether the recipe has enough upstream config to
// be auto-built.
func (u Upstream) HasUpstream() bool {
	return u.Git != "" && u.TagPattern != ""
}

// Watch describes how the manager learns about new upstream versions.
type Watch struct {
	GitHubWebhook bool             `toml:"github_webhook"`
	PollInterval  *config.Duration `toml:"poll_interval"`
}

// recipeView is the TOML decode target for one recipe — only the
// manager-owned sections. peipkg-build's [meta] and [[package]] keys
// surface as md.Undecoded() entries which we ignore.
type recipeView struct {
	Upstream Upstream `toml:"upstream"`
	Watch    Watch    `toml:"watch"`
}

// LoadRoster scans recipesDir, treats every immediate subdirectory
// containing a peipkg.toml as a recipe, and returns them sorted by ID
// for deterministic ordering across runs (logs, webhook routing, etc.).
func LoadRoster(recipesDir string) ([]Recipe, error) {
	entries, err := os.ReadDir(recipesDir)
	if err != nil {
		return nil, fmt.Errorf("read recipes dir %s: %w", recipesDir, err)
	}

	var recipes []Recipe
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(recipesDir, e.Name())
		path := filepath.Join(dir, "peipkg.toml")
		if _, err := os.Stat(path); err != nil {
			if os.IsNotExist(err) {
				// Directory without a peipkg.toml — not a recipe; skip
				// silently. This permits operators to keep ancillary
				// directories under recipes_dir (notes, scratch, etc.)
				// without confusing the loader.
				continue
			}
			return nil, fmt.Errorf("stat %s: %w", path, err)
		}
		r, err := load(path, dir, e.Name())
		if err != nil {
			return nil, err
		}
		recipes = append(recipes, r)
	}

	sort.Slice(recipes, func(i, j int) bool { return recipes[i].ID < recipes[j].ID })
	return recipes, nil
}

func load(path, dir, id string) (Recipe, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Recipe{}, fmt.Errorf("read %s: %w", path, err)
	}
	var view recipeView
	if _, err := toml.Decode(string(data), &view); err != nil {
		return Recipe{}, fmt.Errorf("parse %s: %w", path, err)
	}
	// Note: we deliberately do NOT inspect md.Undecoded(). peipkg-build
	// owns most of the file; we read only what we own and tolerate
	// everything else.
	return Recipe{
		ID:       id,
		Dir:      dir,
		Path:     path,
		Upstream: view.Upstream,
		Watch:    view.Watch,
	}, nil
}
