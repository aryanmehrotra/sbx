package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// A provider that records what it was asked to pull, so the interesting behaviour - what
// prewarm SKIPS and what it reports - can be exercised without a docker daemon.
type fakePuller struct {
	provider.Provider

	present map[string]bool
	pulled  []string
	failOn  string
}

func (f *fakePuller) Name() string { return "fake" }

func (f *fakePuller) Pull(_ context.Context, image string) error {
	if image == f.failOn {
		return errors.New("no such image")
	}

	f.pulled = append(f.pulled, image)

	return nil
}

func (f *fakePuller) HasImage(_ context.Context, tag string) (bool, error) {
	return f.present[tag], nil
}

func (f *fakePuller) Build(context.Context, string, string, string) error { return nil }

// The point of prewarm in CI: a warm cache pulls nothing. If it re-pulls what is already
// there, the step it exists to make cheap is not cheap.
func TestPrewarmSkipsWhatIsAlreadyPresent(t *testing.T) {
	f := &fakePuller{present: map[string]bool{"a:1": true, "b:1": true}}

	var out bytes.Buffer
	if err := Prewarm(context.Background(), f, &out, []string{"a:1", "b:1"}); err != nil {
		t.Fatalf("Prewarm: %v", err)
	}

	if len(f.pulled) != 0 {
		t.Errorf("pulled %v when everything was already present", f.pulled)
	}

	if !strings.Contains(out.String(), "0 pulled, 2 already present") {
		t.Errorf("did not report a fully warm cache:\n%s", out.String())
	}
}

func TestPrewarmPullsWhatIsMissing(t *testing.T) {
	f := &fakePuller{present: map[string]bool{"a:1": true}}

	var out bytes.Buffer
	if err := Prewarm(context.Background(), f, &out, []string{"a:1", "b:1"}); err != nil {
		t.Fatalf("Prewarm: %v", err)
	}

	if len(f.pulled) != 1 || f.pulled[0] != "b:1" {
		t.Errorf("pulled %v, want only b:1", f.pulled)
	}
}

// A prewarm that half-worked leaves a create to fail later on exactly the image that could
// not be fetched. It has to fail here, and name the image, or the prewarm step goes green
// and the failure surfaces somewhere with less context.
func TestPrewarmFailsLoudlyAndNamesTheImage(t *testing.T) {
	f := &fakePuller{present: map[string]bool{}, failOn: "b:1"}

	var out bytes.Buffer

	err := Prewarm(context.Background(), f, &out, []string{"a:1", "b:1"})
	if err == nil {
		t.Fatal("a failed pull was reported as success")
	}

	if !strings.Contains(out.String(), "b:1") {
		t.Errorf("the failing image is not named:\n%s", out.String())
	}

	// The other image still got pulled - one bad image should not abandon the rest.
	if len(f.pulled) != 1 || f.pulled[0] != "a:1" {
		t.Errorf("pulled %v; one failure should not stop the others", f.pulled)
	}
}

// A provider with no Pull is refused by name rather than silently doing nothing.
type noPuller struct{ provider.Provider }

func (noPuller) Name() string { return "kubernetes" }

func TestPrewarmRefusesAProviderThatCannot(t *testing.T) {
	var out bytes.Buffer

	err := Prewarm(context.Background(), noPuller{}, &out, []string{"a:1"})
	if err == nil {
		t.Fatal("a provider with no Pull was accepted")
	}

	if !strings.Contains(err.Error(), "kubernetes") {
		t.Errorf("the refusal does not name the backend: %v", err)
	}
}
