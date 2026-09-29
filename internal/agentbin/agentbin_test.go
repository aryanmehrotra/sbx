package agentbin

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestExplicitBinaryWins(t *testing.T) {
	t.Setenv("SBX_EXECD_BINARY", "/somewhere/sbx")

	src, err := Locate(context.Background(), "arm64", "dev")
	if err != nil || src.File != "/somewhere/sbx" || src.Image != "" {
		t.Fatalf("Locate = %+v, %v", src, err)
	}
}

func TestFindSourceIsThisCheckout(t *testing.T) {
	dir, ok := FindSource()
	if !ok {
		t.Fatal("go test runs inside the checkout, and it was not found")
	}

	if _, err := os.Stat(filepath.Join(dir, "main.go")); err != nil {
		t.Fatalf("%s is not the module root: %v", dir, err)
	}

	// A go.mod naming another module is not ours, however close it is.
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "go.mod"), []byte("module example.com/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if isModuleRoot(other) {
		t.Fatal("another module's root was taken for sbx's")
	}
}

// A release build uses its published artifact, never a cross-compile of whatever checkout is
// nearby; a dev build, which has no published artifact, still compiles its source.
func TestAReleaseNeverCompilesANearbyCheckout(t *testing.T) {
	t.Setenv("SBX_EXECD_BINARY", "")

	if _, ok := FindSource(); !ok {
		t.Skip("needs the checkout")
	}

	saved := crossCompile
	t.Cleanup(func() { crossCompile = saved })

	compiled := 0
	crossCompile = func(context.Context, string, string, string, string) (string, error) {
		compiled++
		return "/built/sbx", nil
	}

	// The other architecture from this one: a linux build asked for its OWN arch returns itself
	// (os.Executable) before any of this, which would pass without testing anything.
	arch := "arm64"
	if runtime.GOARCH == "arm64" {
		arch = "amd64"
	}

	src, err := Locate(context.Background(), arch, "v0.11.0")
	if err != nil || compiled != 0 || src.Image != Activator+":v0.11.0" {
		t.Fatalf("release: %+v, %v, compiled %d times", src, err, compiled)
	}

	if src, err := Locate(context.Background(), arch, "dev"); err != nil || compiled != 1 || src.File != "/built/sbx" {
		t.Fatalf("dev: %+v, %v, compiled %d times", src, err, compiled)
	}
}

// A dev build stamped by `git describe` - v0.15.1-dev+ffd872d - is not "dev", and it is not a
// release either. Only a release has a published activator image; anything else asked docker for
// ghcr.io/...:v0.15.1-dev+ffd872d, which is not even a valid reference ('+'), and the user got
// "invalid reference format" plus a hint to raise ready_timeout_seconds. It must take the dev
// path instead: compile if it can, and otherwise say to set SBX_EXECD_BINARY.
func TestANonReleaseBuildNeverAsksForAPublishedImage(t *testing.T) {
	t.Setenv("SBX_EXECD_BINARY", "")
	t.Setenv("PATH", t.TempDir()) // no go: the cross-compile is not available

	arch := "arm64"
	if runtime.GOARCH == "arm64" {
		arch = "amd64"
	}

	for _, v := range []string{"v0.15.1-dev+ffd872d", "v0.15.1-3-gffd872d", "v0.15.1-dirty", "(devel)"} {
		src, err := Locate(context.Background(), arch, v)
		if src.Image != "" {
			t.Errorf("%s: asked for image %s, which was never published", v, src.Image)
		}

		if err == nil || !strings.Contains(err.Error(), "SBX_EXECD_BINARY") {
			t.Errorf("%s: want the dev-build error naming SBX_EXECD_BINARY, got %v", v, err)
		}
	}
}
