// Package source acquires upstream source trees for a build.
//
// Two operations:
//
//   - ListTags: discover what versions a remote git URL advertises.
//     Used by the poller to detect new upstream versions without
//     downloading anything.
//
//   - Fetch: produce a checked-out work tree at a specific tag.
//     Used by the build orchestrator before invoking peipkg-build.
//
// v0 keeps things simple: every Fetch does a fresh `git clone --depth 1
// --branch <tag>`. No persistent cache, no lockfiles, no incremental
// fetching. A 100-package repo that rebuilds monthly does ~100 clones
// per month — well below the threshold where a cache would pay for
// itself in bandwidth or complexity. If we ever hit that threshold, a
// cache slots in here without changing the public API.
//
// All operations require a working `git` binary on PATH.
package source

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ListTags returns the names of every tag the remote at repoURL
// advertises, in the order git reports them. Network call only — no
// clone, no working tree.
//
// Returned tag names are the short form (`v1.2.3`, not
// `refs/tags/v1.2.3^{}`). Annotated tags' peeled refs (the `^{}`
// suffix) are filtered out so callers see one entry per logical tag.
func ListTags(ctx context.Context, repoURL string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "ls-remote", "--tags", repoURL)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-remote --tags %s: %w", repoURL, err)
	}

	var tags []string
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		// Each line is "<sha>\trefs/tags/<name>" or
		// "<sha>\trefs/tags/<name>^{}" (the peeled ref of an
		// annotated tag). We want the bare names exactly once.
		fields := strings.SplitN(scanner.Text(), "\t", 2)
		if len(fields) != 2 {
			continue
		}
		ref := fields[1]
		if !strings.HasPrefix(ref, "refs/tags/") {
			continue
		}
		name := strings.TrimPrefix(ref, "refs/tags/")
		if strings.HasSuffix(name, "^{}") {
			// Peeled ref — skip; the bare ref already appeared.
			continue
		}
		tags = append(tags, name)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read git ls-remote output: %w", err)
	}
	return tags, nil
}

// Fetch clones repoURL at tag into dst, which must not already exist.
// dst's parent must exist. The clone is shallow (depth 1) so even
// large upstream histories transfer quickly.
//
// On success, dst contains a regular working tree at the requested
// tag with no .git/ directory: the source is presented as upstream
// would ship it (via `git archive` or a release tarball), so build
// scripts that copy the whole tree (`cp -a "$SOURCE_DIR/." "$DESTDIR/"`,
// `make install`, etc.) don't accidentally pull git metadata into
// DESTDIR and trip the orphan check.
//
// Recipes that genuinely need git metadata (e.g., a `git describe` at
// build time) can't currently get it. The expected pattern is to use
// the upstream-supplied version string from the tag rather than
// re-deriving it from git, but if a real recipe needs the metadata,
// Fetch can grow a "keep .git/" option.
func Fetch(ctx context.Context, repoURL, tag, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("destination already exists: %s", dst)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat %s: %w", dst, err)
	}

	cmd := exec.CommandContext(ctx, "git", "clone",
		"--depth", "1",
		"--branch", tag,
		"--single-branch",
		"--no-tags",
		repoURL, dst,
	)
	// Inherit stderr for visibility; clone is operator-relevant noise.
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		// If clone created a partial directory, leave the cleanup to
		// the caller — Fetch's contract is "doesn't exist before,
		// failure may leave junk." That mirrors how operators expect
		// failed builds to be handled.
		return fmt.Errorf("git clone --branch %s %s: %w", tag, repoURL, err)
	}

	// Strip .git/ so the source tree is presented as a clean snapshot.
	if err := os.RemoveAll(filepath.Join(dst, ".git")); err != nil {
		return fmt.Errorf("strip .git/ from cloned source: %w", err)
	}
	return nil
}
