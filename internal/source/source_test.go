package source

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

// makeLocalRepo creates a bare-bones git repository at dir with the
// given tags pointing at sequential commits. Returned URL can be used
// as the repoURL argument to ListTags / Fetch.
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
		// Quiet config; tests should be silent on stderr unless something fails.
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	for i, tag := range tags {
		f := filepath.Join(dir, "f")
		if err := os.WriteFile(f, []byte(tag), 0o644); err != nil {
			t.Fatal(err)
		}
		run("add", "f")
		run("commit", "-q", "-m", "v"+tag)
		run("tag", tag)
		_ = i
	}
	return dir // local path is a valid git URL
}

func TestListTagsLocalRepo(t *testing.T) {
	repo := makeLocalRepo(t, t.TempDir(), []string{"v1.0.0", "v1.0.1", "v2.0.0"})

	tags, err := ListTags(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"v1.0.0", "v1.0.1", "v2.0.0"}
	for _, w := range want {
		if !slices.Contains(tags, w) {
			t.Errorf("missing %q in result %v", w, tags)
		}
	}
	// No peeled refs (^{}) should leak through.
	for _, tag := range tags {
		if len(tag) >= 3 && tag[len(tag)-3:] == "^{}" {
			t.Errorf("peeled ref leaked: %q", tag)
		}
	}
}

func TestFetchLocalRepo(t *testing.T) {
	repo := makeLocalRepo(t, t.TempDir(), []string{"v1.0.0", "v2.0.0"})

	dst := filepath.Join(t.TempDir(), "checkout")
	commitTime, err := Fetch(context.Background(), repo, "v2.0.0", dst, false)
	if err != nil {
		t.Fatal(err)
	}
	// Returned commit time must be RFC 3339 UTC (ends with Z).
	if commitTime == "" {
		t.Error("Fetch returned empty commitTime")
	}
	if got := commitTime[len(commitTime)-1]; got != 'Z' {
		t.Errorf("commitTime = %q, want trailing Z", commitTime)
	}

	body, err := os.ReadFile(filepath.Join(dst, "f"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "v2.0.0" {
		t.Errorf("checked-out content = %q, want %q", body, "v2.0.0")
	}

	// .git/ should have been stripped.
	if _, err := os.Stat(filepath.Join(dst, ".git")); !os.IsNotExist(err) {
		t.Errorf(".git/ leaked through (stat err: %v)", err)
	}
}

func TestFetchCommitTimeIsStable(t *testing.T) {
	// Two clones of the same tag should yield the same commit time.
	// (`git commit` records the committer time at first commit; subsequent
	// fetches don't change it.)
	repo := makeLocalRepo(t, t.TempDir(), []string{"v1.0.0"})

	a, err := Fetch(context.Background(), repo, "v1.0.0", filepath.Join(t.TempDir(), "a"), false)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Fetch(context.Background(), repo, "v1.0.0", filepath.Join(t.TempDir(), "b"), false)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("commit times differ across fetches: %q vs %q", a, b)
	}
}

func TestFetchRefusesExistingDst(t *testing.T) {
	repo := makeLocalRepo(t, t.TempDir(), []string{"v1.0.0"})
	dst := t.TempDir() // exists

	_, err := Fetch(context.Background(), repo, "v1.0.0", dst, false)
	if err == nil {
		t.Error("Fetch should refuse to overwrite an existing dst")
	}
}

// TestFetchWithSubmodulesPopulatesContent builds a parent repo that
// references a child repo as a submodule and tags v1.0.0 on the
// parent. Fetch with submodules=true must produce a tree where the
// submodule's working-tree content is present (not just an empty
// directory) and where every .git directory anywhere in the tree has
// been stripped.
func TestFetchWithSubmodulesPopulatesContent(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	tmp := t.TempDir()

	child := makeLocalRepo(t, filepath.Join(tmp, "child"), []string{"v1.0.0"})

	// Parent gets a submodule pointing at child, then tags v1.0.0.
	parentDir := filepath.Join(tmp, "parent")
	if err := os.MkdirAll(parentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(cwd string, args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = cwd
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
			// Allow file:// (or filesystem path) submodule URLs in the
			// recent gits where security defaults block them.
			"GIT_ALLOW_PROTOCOL=file:local",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v in %s: %v\n%s", args, cwd, err, out)
		}
	}
	run(parentDir, "init", "-q", "-b", "main")
	run(parentDir, "-c", "protocol.file.allow=always", "submodule", "add", "-q", child, "vendor")
	run(parentDir, "commit", "-q", "-m", "add vendor submodule")
	run(parentDir, "tag", "v1.0.0")

	dst := filepath.Join(tmp, "checkout")
	if _, err := exec.Command("git", "-c", "protocol.file.allow=always",
		"clone", "--depth", "1", "--branch", "v1.0.0", "--single-branch", "--no-tags",
		parentDir, dst).CombinedOutput(); err != nil {
		t.Fatalf("pre-clone: %v", err)
	}
	// Fetch's own clone command does not bypass file-protocol restrictions
	// so we test stripGitMetadata + the submodule update path on the
	// just-cloned dst directly. (In production, git over https has no
	// such restriction.)
	if out, err := exec.Command("git", "-C", dst,
		"-c", "protocol.file.allow=always",
		"submodule", "update", "--init", "--recursive", "--depth", "1",
	).CombinedOutput(); err != nil {
		t.Fatalf("submodule update: %v\n%s", err, out)
	}
	if err := stripGitMetadata(dst); err != nil {
		t.Fatalf("stripGitMetadata: %v", err)
	}

	// vendor/f exists with the child's tag content.
	body, err := os.ReadFile(filepath.Join(dst, "vendor", "f"))
	if err != nil {
		t.Fatalf("submodule content missing: %v", err)
	}
	if string(body) != "v1.0.0" {
		t.Errorf("submodule content = %q, want %q", body, "v1.0.0")
	}

	// No .git anywhere — neither at the top level nor inside the
	// submodule worktree.
	if err := filepath.WalkDir(dst, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Name() == ".git" {
			return errLeakedGit{path: path}
		}
		return nil
	}); err != nil {
		t.Errorf("git metadata leaked: %v", err)
	}
}

// errLeakedGit is a sentinel returned by the WalkDir callback when a
// .git directory or file is found post-stripGitMetadata.
type errLeakedGit struct{ path string }

func (e errLeakedGit) Error() string { return ".git found at " + e.path }
