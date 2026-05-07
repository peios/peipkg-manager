// Package publish folds new .peipkg files into the repository state by
// shelling out to peipkg-repo, and (optionally) uploads the resulting
// state to a remote object store via rclone.
//
// As with internal/build, this package is intentionally thin: the
// caller decides when to publish and which files to include. It does
// not retry, batch, or schedule.
package publish

import (
	"context"
	"fmt"
	"os"
	"os/exec"
)

// Publisher holds invocation-time configuration shared across publish
// calls.
type Publisher struct {
	// PeipkgRepoPath is the absolute path to the peipkg-repo binary,
	// resolved at startup and pinned here.
	PeipkgRepoPath string

	// RepoDir is the repository state directory. Publish runs in-place:
	// reads from RepoDir, writes back to RepoDir.
	RepoDir string

	// SignKeyPath is the Ed25519 private key peipkg-repo signs the
	// new descriptor and indexes with.
	SignKeyPath string

	// UploadBackend is "rclone" or "" (no upload). Empty backend means
	// the published state stays local; an external script handles the
	// upload, or it's a development/testing setup.
	UploadBackend string
	UploadRemote  string
}

// Publish ingests the .peipkg files in newPackagesDir into the
// repository state, signs the new indexes with timestamp, and (if
// configured) uploads the resulting state to the remote.
//
// timestamp must be RFC 3339 UTC ending in 'Z'; it is recorded as the
// new active and archive indexes' generated_at field. The caller picks
// the value (typically the current UTC time at publish call).
func (p *Publisher) Publish(ctx context.Context, newPackagesDir, timestamp string) error {
	args := []string{
		"publish",
		"--in", p.RepoDir,
		"--new", newPackagesDir,
		"--out", p.RepoDir,
		"--sign-key", p.SignKeyPath,
		"--timestamp", timestamp,
	}
	cmd := exec.CommandContext(ctx, p.PeipkgRepoPath, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("peipkg-repo publish: %w", err)
	}

	if p.UploadBackend == "rclone" {
		if err := p.rcloneSync(ctx); err != nil {
			return fmt.Errorf("rclone sync: %w", err)
		}
	}
	return nil
}

// Init creates a fresh repository state at p.RepoDir if one does not
// already exist. Idempotent: a populated RepoDir is left alone. Used
// by the manager on first start so operators don't have to run
// peipkg-repo init manually.
func (p *Publisher) Init(ctx context.Context, name, description, timestamp string) error {
	// Detect existing state by checking for repo.json.
	if _, err := os.Stat(p.RepoDir + "/repo.json"); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat existing repo: %w", err)
	}

	if err := os.MkdirAll(p.RepoDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", p.RepoDir, err)
	}

	args := []string{
		"init",
		"--name", name,
		"--sign-key", p.SignKeyPath,
		"--timestamp", timestamp,
		"--out", p.RepoDir,
	}
	if description != "" {
		args = append(args, "--description", description)
	}
	cmd := exec.CommandContext(ctx, p.PeipkgRepoPath, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("peipkg-repo init: %w", err)
	}
	return nil
}

// rcloneSync mirrors p.RepoDir to p.UploadRemote. Order of upload is
// the operator's responsibility — for object stores like R2 we want
// .peipkg files to land before indexes (so consumers don't briefly see
// an index referencing a not-yet-uploaded file). rclone sync uploads
// in roughly arbitrary order; for v0 we accept the brief inconsistency
// window in exchange for not building our own ordered uploader.
//
// A future improvement is a two-pass upload: pass 1 uploads everything
// EXCEPT repo.json + repo.json.sig + index/*.json + index/*.json.sig,
// pass 2 uploads those last. The split is mechanical and the cost is
// twice as many remote API calls; not worth the complexity at v0
// scale (~hundreds of objects).
func (p *Publisher) rcloneSync(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "rclone", "sync", p.RepoDir, p.UploadRemote)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
