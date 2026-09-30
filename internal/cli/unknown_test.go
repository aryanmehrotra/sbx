package cli

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// fleet lists whatever sandboxes it was given, filtered by name the way a provider does.
type fleet struct {
	provider.Provider
	all []provider.Unit
}

func (f *fleet) Name() string { return "docker" }
func (f *fleet) List(_ context.Context, sandbox string) ([]provider.Unit, error) {
	var out []provider.Unit

	for _, u := range f.all {
		if sandbox == "" || u.Sandbox == sandbox {
			out = append(out, u)
		}
	}

	return out, nil
}

func fleetOf(n int) *fleet {
	f := &fleet{}
	for i := range n {
		f.all = append(f.all, units(fmt.Sprintf("feature-%02d", i), "db", "web")...)
	}

	return f
}

// One format, whatever the count. Past eight sandboxes the error used to switch to "There are
// N others - `sbx list` names them", so the same typo read two different ways on two machines,
// and on the busier one - where a typo is likeliest - it sent the reader off to run a command
// to find the name the error already had in hand.
func TestUnknownSandboxAlwaysNamesTheOnesThatExist(t *testing.T) {
	for _, n := range []int{3, 12} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			err := UnknownSandbox(context.Background(), fleetOf(n), "feture-01")
			if err == nil {
				t.Fatal("no error for a sandbox that does not exist")
			}

			msg := err.Error()
			if !strings.Contains(msg, `no sandbox "feture-01". These exist: `) {
				t.Errorf("not the one format:\n%s", msg)
			}

			for i := range n {
				if name := fmt.Sprintf("feature-%02d", i); !strings.Contains(msg, name) {
					t.Errorf("%s exists and the error does not name it:\n%s", name, msg)
				}
			}

			// Deduplicated: List returns one unit per service.
			if c := strings.Count(msg, "feature-00"); c != 1 {
				t.Errorf("feature-00 named %d times:\n%s", c, msg)
			}
		})
	}
}

// `sbx url typo web` answered "no service "web" in sandbox "typo"" - a third format, and one
// that blames the service for a mistyped sandbox.
func TestWakePortOnAnUnknownSandboxSaysSo(t *testing.T) {
	_, err := WakePort(context.Background(), fleetOf(2), "feture-01", "web")
	if err == nil || !strings.Contains(err.Error(), `no sandbox "feture-01". These exist: feature-00, feature-01`) {
		t.Errorf("want the unknown-sandbox error, got %v", err)
	}
}
