package fchost

import (
	"context"
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// Every path that acts on "where does a microVM run from here" must get the same answer from the
// same machine: the provider, the CLI redirect, `sbx serve`/`sbx fc` (helperManager) and doctor.
// The hosts below are the ones two detectors used to disagree about: an M3+ Mac (doctor's
// second row said ✗), a chip only SBX_FC_ASSUME_NESTED lets through, a Mac with no VM tool, and
// Windows - hostcap alone called all four something other than what the redirect did.
func TestEveryPathActsOnTheSameDecision(t *testing.T) {
	saved := hostProbe
	t.Cleanup(func() { hostProbe = saved })

	future := mac("Apple Silicon Mystery", "27.0", "limactl")
	future.env = map[string]string{AssumeNestedEnv: "1"}

	win := fakeHost{goos: "windows", goarch: "amd64",
		out:  map[string]string{"cmd /c ver": "Microsoft Windows [Version 10.0.26100.1]"},
		path: map[string]bool{"wsl.exe": true}}

	for _, tc := range []struct {
		name string
		host fakeHost
		want Kind
	}{
		{"M4 with lima", mac("Apple M4", "26.4.1", "limactl"), HelperVM},
		{"unknown chip, assume nested", future, HelperVM},
		{"M4 without a VM tool", mac("Apple M4", "26.4.1"), Refused},
		{"M2", mac("Apple M2", "26.4.1", "limactl"), Refused},
		{"windows 11", win, HelperVM},
		{"linux without kvm", fakeHost{goos: "linux", goarch: "amd64"}, Refused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hostProbe = tc.host.probe

			b := HostBackend()
			if b.Kind != tc.want {
				t.Fatalf("HostBackend = %s (%s), want %s", b.Kind, b.Reason, tc.want)
			}

			// The provider.
			if d := provider.DecideHost(); d.Backend != b.Kind || d.Reason != b.Reason || d.Next != b.Next {
				t.Fatalf("provider decided %+v, HostBackend %+v", d, b)
			}

			p, err := provider.For("firecracker", "", "")

			switch b.Kind {
			case HelperVM:
				if _, ok := p.(*Remote); !ok || err != nil {
					t.Fatalf("provider = %T, %v; want the helper VM's Remote", p, err)
				}
			case Refused:
				if err == nil || !strings.Contains(err.Error(), b.Reason) {
					t.Fatalf("provider err = %v, want the refusal %q", err, b.Reason)
				}
			}

			// `sbx serve` and `sbx fc vm`.
			m, err := helperManager(nil)
			if (b.Kind == HelperVM) != (err == nil) {
				t.Fatalf("helperManager err = %v for %s", err, b.Kind)
			}

			if m != nil && m.Driver.Name() != b.Helper {
				t.Fatalf("helperManager drives %s, the decision named %s", m.Driver.Name(), b.Helper)
			}

			// The redirect: a refusal is handled here with exit 1; only Direct falls through.
			if b.Kind == Refused {
				if handled, code := Redirect(context.Background(), "dev", "list", nil); !handled || code != 1 {
					t.Fatalf("Redirect on a refused host = %v, %d", handled, code)
				}
			}

			// doctor.
			have, detail, _ := DoctorRow(context.Background(), b, "sbx-fc", func(context.Context) (State, error) { return Absent, nil })
			if have != (b.Kind == HelperVM) || !strings.Contains(detail, b.Reason) {
				t.Fatalf("doctor = %v %q for %s", have, detail, b.Kind)
			}
		})
	}
}
