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
//   - Shut down cleanly on SIGTERM/SIGINT (cancel context, drain
//     in-flight build, exit).
package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/peios/peipkg-manager/internal/build"
	"github.com/peios/peipkg-manager/internal/config"
	"github.com/peios/peipkg-manager/internal/publish"
	"github.com/peios/peipkg-manager/internal/recipe"
	"github.com/peios/peipkg-manager/internal/watch"
)

// Manager is the assembled daemon. Construct via New, run via Run.
type Manager struct {
	cfg     config.Config
	logger  *slog.Logger
	recipes []recipe.Recipe

	runner    *build.Runner
	publisher *publish.Publisher
	watcher   *watch.Watcher

	repoDir    string // <state_dir>/repo, kept here so dedup checks don't have to reach through publisher
	stagingDir string // where successful build outputs are accumulated before publish

	// Webhook secret loaded once from disk; nil if no HTTP server.
	webhookSecret string
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
	}, nil
}

// Run is the long-running daemon loop. It returns when ctx is
// cancelled, after waiting for the in-flight build (if any) to
// complete and shutting down the watcher.
func (m *Manager) Run(ctx context.Context) error {
	if err := m.ensureRepoInitialised(ctx); err != nil {
		return fmt.Errorf("initialise repo: %w", err)
	}

	triggers := make(chan watch.Trigger, 1024)
	m.watcher = &watch.Watcher{
		Recipes:       m.recipes,
		DefaultPoll:   m.cfg.Poll.DefaultInterval.Duration,
		WebhookSecret: m.webhookSecret,
		HTTPAddr:      m.cfg.HTTP.Addr,
		Logger:        m.logger,
		Triggers:      triggers,
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := m.watcher.Run(ctx); err != nil {
			m.logger.Error("watcher exited", "err", err)
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
// the archive, build, publish on success.
//
// Build failures are logged and dropped — the next poll cycle will
// re-emit a trigger for the same tag, giving a natural retry. Publish
// failures are logged but do not undo the build (the .peipkg sits in
// the publish staging dir for next attempt).
func (m *Manager) handleTrigger(ctx context.Context, trig watch.Trigger) {
	version := composeVersion(trig.Captured, trig.Recipe.Upstream.PeiosRevision)

	already, err := m.alreadyPublished(trig.Recipe.ID, version)
	if err != nil {
		m.logger.Warn("dedup check failed; proceeding with build", "err", err)
	} else if already {
		m.logger.Debug("trigger skipped (already published)",
			"recipe", trig.Recipe.ID, "version", version, "source", trig.Source)
		return
	}

	m.logger.Info("starting build",
		"recipe", trig.Recipe.ID, "tag", trig.UpstreamTag,
		"version", version, "source", trig.Source)

	job := build.Job{
		Recipe:      trig.Recipe,
		UpstreamRef: trig.UpstreamTag,
		Version:     version,
		SourceRef:   composeSourceRef(trig.Recipe.Upstream.Git, trig.UpstreamTag),
		FarmID:      m.cfg.Manager.ID,
		Timestamp:   nowRFC3339(),
		SignKeyPath: m.cfg.Signing.KeyFile,
	}
	res, err := m.runner.Run(ctx, job)
	if err != nil {
		m.logger.Error("build failed", "recipe", trig.Recipe.ID, "version", version, "err", err)
		return
	}

	// Move outputs from the per-build stage dir into the shared
	// publish-staging dir so peipkg-repo publish picks them up.
	if err := m.stageForPublish(res.Outputs); err != nil {
		m.logger.Error("stage outputs failed", "err", err)
		return
	}

	if err := m.publisher.Publish(ctx, m.stagingDir, nowRFC3339()); err != nil {
		m.logger.Error("publish failed", "err", err)
		// Outputs remain in stagingDir; next successful build will
		// publish them along with its own.
		return
	}

	// Publish succeeded — clear stagingDir so the next build doesn't
	// re-publish the same files.
	if err := clearDir(m.stagingDir); err != nil {
		m.logger.Warn("clear staging dir failed", "err", err)
	}

	m.logger.Info("build + publish complete",
		"recipe", trig.Recipe.ID, "version", version,
		"outputs", len(res.Outputs))
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
