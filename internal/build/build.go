// Package build orchestrates one peipkg-build invocation: fetch source,
// shell out to peipkg-build with the right flags, return the produced
// .peipkg paths.
//
// The orchestrator is intentionally thin. It does not retry, does not
// poll, does not aggregate jobs. The caller (internal/manager) decides
// when to run a job and what to do with the outputs. Each Run is a
// single self-contained build attempt.
package build

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/peios/peipkg-manager/internal/recipe"
	"github.com/peios/peipkg-manager/internal/source"
)

// Job describes one build to run.
//
// The build timestamp is intentionally NOT a Job field. Runner derives
// it from the upstream tag's committer timestamp during source fetch,
// so the same (recipe, upstream tag, peios revision) tuple produces
// byte-identical .peipkg output regardless of when the build runs —
// even years apart.
type Job struct {
	Recipe      recipe.Recipe
	UpstreamRef string // git tag, e.g. "v1.3.2"
	Version     string // peipkg version string composed by the caller, e.g. "1.3.2-1"
	SourceRef   string // recorded in the manifest's build.source_ref (PSD-009 §3.3.4)
	FarmID      string
	SignKeyPath string // empty = unsigned package
}

// Result reports the outputs of a successful build.
type Result struct {
	// Outputs is the list of absolute paths to .peipkg files
	// peipkg-build produced. A multi-stanza recipe produces multiple
	// outputs from one build.
	Outputs []string

	// Timestamp is the RFC 3339 UTC timestamp the build used as
	// peipkg-build's --timestamp flag, derived from the upstream
	// tag's committer timestamp. Surfaced for logging and audit.
	Timestamp string
}

// Runner holds invocation-time settings shared across builds.
type Runner struct {
	// PeipkgBuildPath is the absolute path to the peipkg-build binary.
	// The manager resolves it via PATH at startup and pins it here so
	// individual builds cannot be hijacked by a later PATH change.
	PeipkgBuildPath string

	// SourcesBaseDir is the parent of per-build source checkouts. The
	// runner creates and cleans up subdirectories here.
	SourcesBaseDir string

	// StageBaseDir is the parent of per-build output directories. The
	// runner creates a subdirectory per Run; the caller is responsible
	// for moving the outputs out and removing the stage dir when done.
	StageBaseDir string
}

// Run executes one build. On success it returns the produced .peipkg
// paths (which live under StageBaseDir/<recipe-id>-<version>/). On
// failure it removes the partial stage directory but otherwise leaves
// no trace.
//
// The source checkout is unconditionally removed on return: source
// trees are large, ephemeral, and we don't want them accumulating in
// the manager's state directory.
func (r *Runner) Run(ctx context.Context, job Job) (*Result, error) {
	jobID := job.Recipe.ID + "-" + job.Version
	sourceDir := filepath.Join(r.SourcesBaseDir, jobID)
	stageDir := filepath.Join(r.StageBaseDir, jobID)

	// Both directories are per-build. If a previous run left something
	// behind, clear it.
	if err := os.RemoveAll(sourceDir); err != nil {
		return nil, fmt.Errorf("clear stale source %s: %w", sourceDir, err)
	}
	if err := os.RemoveAll(stageDir); err != nil {
		return nil, fmt.Errorf("clear stale stage %s: %w", stageDir, err)
	}
	if err := os.MkdirAll(r.SourcesBaseDir, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", r.SourcesBaseDir, err)
	}
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", stageDir, err)
	}

	commitTime, err := source.Fetch(ctx, job.Recipe.Upstream.Git, job.UpstreamRef, sourceDir, job.Recipe.Upstream.Submodules)
	if err != nil {
		_ = os.RemoveAll(stageDir)
		return nil, fmt.Errorf("fetch source: %w", err)
	}
	defer os.RemoveAll(sourceDir)

	args := []string{
		"build",
		"--recipe", job.Recipe.Path,
		"--source", sourceDir,
		"--version", job.Version,
		"--source-ref", job.SourceRef,
		"--farm-id", job.FarmID,
		"--timestamp", commitTime,
		"--out", stageDir,
	}
	if job.SignKeyPath != "" {
		args = append(args, "--sign-key", job.SignKeyPath)
	}

	cmd := exec.CommandContext(ctx, r.PeipkgBuildPath, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		_ = os.RemoveAll(stageDir)
		return nil, fmt.Errorf("peipkg-build %s %s: %w", job.Recipe.ID, job.Version, err)
	}

	outputs, err := collectOutputs(stageDir)
	if err != nil {
		return nil, err
	}
	if len(outputs) == 0 {
		return nil, fmt.Errorf("peipkg-build succeeded but produced no .peipkg files in %s", stageDir)
	}
	return &Result{Outputs: outputs, Timestamp: commitTime}, nil
}

func collectOutputs(stageDir string) ([]string, error) {
	entries, err := os.ReadDir(stageDir)
	if err != nil {
		return nil, fmt.Errorf("read stage dir %s: %w", stageDir, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if filepath.Ext(e.Name()) != ".peipkg" {
			continue
		}
		out = append(out, filepath.Join(stageDir, e.Name()))
	}
	return out, nil
}
