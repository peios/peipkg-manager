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
	if err := Fetch(context.Background(), repo, "v2.0.0", dst); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(filepath.Join(dst, "f"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "v2.0.0" {
		t.Errorf("checked-out content = %q, want %q", body, "v2.0.0")
	}
}

func TestFetchRefusesExistingDst(t *testing.T) {
	repo := makeLocalRepo(t, t.TempDir(), []string{"v1.0.0"})
	dst := t.TempDir() // exists

	err := Fetch(context.Background(), repo, "v1.0.0", dst)
	if err == nil {
		t.Error("Fetch should refuse to overwrite an existing dst")
	}
}
