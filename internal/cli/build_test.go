package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/spec"
)

func ctxDir(t *testing.T, files map[string]string) string {
	t.Helper()

	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	return dir
}

func tagOf(t *testing.T, dir string) string {
	t.Helper()

	tag, err := BuildTag(filepath.Dir(dir), &spec.Build{Context: filepath.Base(dir)})
	if err != nil {
		t.Fatalf("BuildTag: %v", err)
	}

	return tag
}

// The point of hashing content: the same inputs are the same image, so the build is skipped.
func TestSameContentSameTag(t *testing.T) {
	a := ctxDir(t, map[string]string{"Dockerfile": "FROM nginx\n", "app.txt": "hello"})
	b := ctxDir(t, map[string]string{"Dockerfile": "FROM nginx\n", "app.txt": "hello"})

	if tagOf(t, a) != tagOf(t, b) {
		t.Errorf("identical contexts produced different tags:\n  %s\n  %s", tagOf(t, a), tagOf(t, b))
	}
}

// And the other half: a changed byte is a different image, or the cache serves something
// that is not what the file describes.
func TestChangedContentChangesTag(t *testing.T) {
	a := ctxDir(t, map[string]string{"Dockerfile": "FROM nginx\n"})
	before := tagOf(t, a)

	if err := os.WriteFile(filepath.Join(a, "Dockerfile"), []byte("FROM nginx:alpine\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if tagOf(t, a) == before {
		t.Error("changing the Dockerfile did not change the tag")
	}
}

// The reason this hashes content rather than timestamps. A fresh clone rewrites every
// mtime, and a time-based key would miss the cache on every CI runner — which is exactly
// where it is worth the most.
func TestTouchingAFileDoesNotChangeTheTag(t *testing.T) {
	a := ctxDir(t, map[string]string{"Dockerfile": "FROM nginx\n", "app.txt": "hello"})
	before := tagOf(t, a)

	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(filepath.Join(a, "app.txt"), old, old); err != nil {
		t.Fatal(err)
	}

	if tagOf(t, a) != before {
		t.Error("changing an mtime changed the tag — the cache would miss on every fresh clone")
	}
}

// A script that stops being executable is a different image, and would otherwise be a
// silent cache hit that fails at runtime.
func TestModeChangesTag(t *testing.T) {
	a := ctxDir(t, map[string]string{"Dockerfile": "FROM nginx\n", "run.sh": "#!/bin/sh\n"})
	before := tagOf(t, a)

	if err := os.Chmod(filepath.Join(a, "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}

	if tagOf(t, a) == before {
		t.Error("making a file executable did not change the tag")
	}
}

// .git is excluded, or the tag changes on every commit whether or not a build input did.
func TestGitDirIsIgnored(t *testing.T) {
	a := ctxDir(t, map[string]string{"Dockerfile": "FROM nginx\n"})
	before := tagOf(t, a)

	if err := os.MkdirAll(filepath.Join(a, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(a, ".git", "HEAD"), []byte("ref: refs/heads/main"), 0o644); err != nil {
		t.Fatal(err)
	}

	if tagOf(t, a) != before {
		t.Error("a .git directory changed the tag — every commit would bust the cache")
	}
}

func TestMissingContextIsAnError(t *testing.T) {
	if _, err := BuildTag(t.TempDir(), &spec.Build{Context: "nope"}); err == nil {
		t.Error("a missing build context was accepted")
	}
}
