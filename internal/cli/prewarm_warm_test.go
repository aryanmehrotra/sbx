package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// fakeWarmer is a provider whose create needs more than the image - a microVM's root filesystem.
type fakeWarmer struct {
	provider.Provider

	cached map[string]bool
	warmed []string
	pulled []string
}

func (f *fakeWarmer) Name() string { return "firecracker" }

func (f *fakeWarmer) Pull(_ context.Context, image string) error {
	f.pulled = append(f.pulled, image)
	return nil
}

func (f *fakeWarmer) Warm(_ context.Context, image string) (bool, error) {
	if image == "bad:1" {
		return false, errors.New("mkfs.ext4 failed")
	}

	f.warmed = append(f.warmed, image)

	return !f.cached[image], nil
}

// On firecracker the slow part of a first create is not the pull but building the image's root
// filesystem (docker export + mkfs.ext4) - 2.5 GB for the code interpreter, past the SDK's wait.
// Prewarm does that part, and says which images were already built.
func TestPrewarmBuildsAMicroVMRootfsNotJustThePull(t *testing.T) {
	f := &fakeWarmer{cached: map[string]bool{"python:3.11-slim": true}}

	var out bytes.Buffer
	if err := Prewarm(context.Background(), f, &out, []string{"python:3.11-slim", "opensandbox/code-interpreter:v1"}); err != nil {
		t.Fatal(err)
	}

	if len(f.warmed) != 2 || len(f.pulled) != 0 {
		t.Fatalf("warmed %v, pulled %v: want both warmed, and warming to be the whole job", f.warmed, f.pulled)
	}

	if s := out.String(); !strings.Contains(s, "1 built, 1 already present") {
		t.Fatalf("prewarm did not say what it built:\n%s", s)
	}

	out.Reset()

	if err := Prewarm(context.Background(), f, &out, []string{"bad:1"}); err == nil || !strings.Contains(out.String(), "mkfs.ext4 failed") {
		t.Fatalf("a failed build was not reported by name: %v\n%s", err, out.String())
	}
}
