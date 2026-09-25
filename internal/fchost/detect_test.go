package fchost

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/fc/hostcap"
)

// fakeHost is a machine described by what its tools would print. Every detection path runs
// on every GOOS through it, so the darwin branch is tested on linux CI and the windows branch
// is tested here, where there is no Windows host at all.
type fakeHost struct {
	goos, goarch string
	out          map[string]string // "name arg arg" -> stdout; absent means the command failed
	files        map[string]string
	devs         map[string]bool
	path         map[string]bool
	env          map[string]string
}

func (f fakeHost) probe() Probe {
	return Probe{
		GOOS: f.goos, GOARCH: f.goarch,
		Output: func(name string, args ...string) (string, error) {
			key := strings.Join(append([]string{name}, args...), " ")
			if v, ok := f.out[key]; ok {
				return v, nil
			}

			return "", errors.New("exit status 1")
		},
		ReadFile: func(p string) ([]byte, error) {
			if v, ok := f.files[p]; ok {
				return []byte(v), nil
			}

			return nil, errors.New("no such file")
		},
		CharDevice: func(p string) bool { return f.devs[p] },
		LookPath:   func(n string) bool { return f.path[n] },
		Getenv:     func(k string) string { return f.env[k] },
		Home:       "/home/me",
	}
}

func mac(chip, ver string, tools ...string) fakeHost {
	path := map[string]bool{}
	for _, t := range tools {
		path[t] = true
	}

	return fakeHost{
		goos: "darwin", goarch: "arm64",
		out: map[string]string{
			"sysctl -n machdep.cpu.brand_string": chip + "\n",
			"sw_vers -productVersion":            ver + "\n",
		},
		path: path,
	}
}

func TestDarwinBackend(t *testing.T) {
	cases := []struct {
		name   string
		host   fakeHost
		kind   Kind
		helper string
		want   string // in Reason+Next
	}{
		// The machine this was written on: the spike measured /dev/kvm through colima here.
		{"M4 on 26 with lima", mac("Apple M4", "26.4.1", "limactl", "colima"), HelperVM, "lima", "Apple M4"},
		{"M3 Pro on 15.0 with colima only", mac("Apple M3 Pro", "15.0", "colima"), HelperVM, "colima", "macOS 15.0"},
		{"M2 is too old", mac("Apple M2 Max", "15.2", "limactl"), Refused, "", "M3 and later"},
		{"M1 is too old", mac("Apple M1", "14.6", "limactl"), Refused, "", "M3 and later"},
		{"macOS 14 is too old", mac("Apple M3", "14.7.1", "limactl"), Refused, "", "macOS 15"},
		{"no VM tool", mac("Apple M4", "26.4.1"), Refused, "", "brew install lima"},
		{"a chip name it cannot read", mac("Apple Silicon Z", "26.0", "limactl"), Refused, "", "SBX_FC_ASSUME_NESTED"},
		{"intel", fakeHost{goos: "darwin", goarch: "amd64", path: map[string]bool{"limactl": true}}, Refused, "", "Intel"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := Detect(c.host.probe())
			if b.Kind != c.kind {
				t.Fatalf("kind = %s (%s / %s), want %s", b.Kind, b.Reason, b.Next, c.kind)
			}

			if b.Helper != c.helper {
				t.Errorf("helper = %q, want %q", b.Helper, c.helper)
			}

			if !strings.Contains(b.Reason+" "+b.Next, c.want) {
				t.Errorf("%q / %q does not mention %q", b.Reason, b.Next, c.want)
			}

			if b.Kind == Refused && b.Next == "" {
				t.Error("a refusal must say what to do next")
			}
		})
	}
}

// The override exists for a chip name this code has never seen - it must not also unlock a
// chip it has seen and knows to be too old.
func TestAssumeNestedOnlyCoversAnUnreadableChip(t *testing.T) {
	h := mac("Apple Silicon Z", "26.0", "limactl")
	h.env = map[string]string{"SBX_FC_ASSUME_NESTED": "1"}

	if b := Detect(h.probe()); b.Kind != HelperVM {
		t.Fatalf("unreadable chip + override = %s (%s)", b.Kind, b.Reason)
	}

	h = mac("Apple M1", "26.0", "limactl")
	h.env = map[string]string{"SBX_FC_ASSUME_NESTED": "1"}

	if b := Detect(h.probe()); b.Kind != Refused {
		t.Fatalf("M1 + override = %s, want refused", b.Kind)
	}
}

func TestDriverChoiceCanBeForced(t *testing.T) {
	h := mac("Apple M4", "26.4.1", "limactl", "colima")
	h.env = map[string]string{"SBX_FC_VM_DRIVER": "colima"}

	if b := Detect(h.probe()); b.Helper != "colima" {
		t.Fatalf("helper = %q, want colima", b.Helper)
	}

	h.env["SBX_FC_VM_DRIVER"] = "qemu"
	if b := Detect(h.probe()); b.Kind != Refused || !strings.Contains(b.Next, "lima or colima") {
		t.Fatalf("an unknown driver must be refused, got %s %q", b.Kind, b.Next)
	}

	h = mac("Apple M4", "26.4.1", "limactl")
	h.env = map[string]string{"SBX_FC_VM_DRIVER": "colima"}

	if b := Detect(h.probe()); b.Kind != Refused || !strings.Contains(b.Reason, "colima") {
		t.Fatalf("asking for a driver that is not installed = %s %q", b.Kind, b.Reason)
	}
}

func TestLinuxBackend(t *testing.T) {
	withKVM := fakeHost{goos: "linux", goarch: "amd64", devs: map[string]bool{"/dev/kvm": true}}
	if b := Detect(withKVM.probe()); b.Kind != Direct {
		t.Fatalf("/dev/kvm present = %s, want direct", b.Kind)
	}

	cases := []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{"plain cloud VM", nil, []string{"/dev/kvm", "nested virtualisation", "bare-metal", "--isolation gvisor|kata"}},
		{"EC2", map[string]string{"/sys/class/dmi/id/sys_vendor": "Amazon EC2\n"}, []string{"Amazon EC2", ".metal"}},
		{"GCE", map[string]string{"/sys/class/dmi/id/sys_vendor": "Google\n"}, []string{"--enable-nested-virtualization"}},
		{"Azure", map[string]string{"/sys/class/dmi/id/sys_vendor": "Microsoft Corporation\n"}, []string{"Azure", "nested virtualisation"}},
		{"inside WSL", map[string]string{"/proc/version": "Linux version 5.15.167.4-microsoft-standard-WSL2"}, []string{"nestedVirtualization=true", ".wslconfig"}},
		{"CPU has vmx but no module", map[string]string{"/proc/cpuinfo": "flags : fpu vmx sse\n"}, []string{"modprobe kvm"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := fakeHost{goos: "linux", goarch: "amd64", files: c.files}

			b := Detect(h.probe())
			if b.Kind != Refused {
				t.Fatalf("no /dev/kvm = %s, want refused (never a silent fallback)", b.Kind)
			}

			all := b.Reason + " " + b.Next
			for _, w := range c.want {
				if !strings.Contains(all, w) {
					t.Errorf("%q does not mention %q", all, w)
				}
			}
		})
	}
}

func win(ver, wslconfig string, wsl bool) fakeHost {
	h := fakeHost{
		goos: "windows", goarch: "amd64",
		out:   map[string]string{"cmd /c ver": "\r\nMicrosoft Windows [Version " + ver + "]\r\n"},
		files: map[string]string{},
		path:  map[string]bool{"wsl.exe": wsl},
	}

	if wslconfig != "" {
		h.files[`/home/me/.wslconfig`] = wslconfig
	}

	return h
}

func TestWindowsBackend(t *testing.T) {
	cases := []struct {
		name string
		host fakeHost
		kind Kind
		want string
	}{
		{"win11, no .wslconfig: nested is on by default", win("10.0.22631.4317", "", true), HelperVM, "WSL2"},
		{"win11, explicitly on", win("10.0.26100.1", "[wsl2]\nnestedVirtualization=true\n", true), HelperVM, "WSL2"},
		{"win11, turned off", win("10.0.22631.4317", "[wsl2]\r\nmemory=8GB\r\nnestedVirtualization = false\r\n", true), Refused, "nestedVirtualization=true"},
		{"win11, off but in another section", win("10.0.22631.4317", "[experimental]\nnestedVirtualization=false\n", true), HelperVM, "WSL2"},
		{"windows 10", win("10.0.19045.3803", "", true), Refused, "Windows 11"},
		{"no WSL", win("10.0.22631.4317", "", false), Refused, "wsl --install"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := Detect(c.host.probe())
			if b.Kind != c.kind {
				t.Fatalf("kind = %s (%s / %s), want %s", b.Kind, b.Reason, b.Next, c.kind)
			}

			if !strings.Contains(b.Reason+" "+b.Next, c.want) {
				t.Errorf("%q / %q does not mention %q", b.Reason, b.Next, c.want)
			}

			if b.Kind == HelperVM && b.Helper != "wsl" {
				t.Errorf("helper = %q, want wsl", b.Helper)
			}
		})
	}
}

// The refusal must carry the exact line, section included, so it can be pasted.
func TestWSLRefusalNamesTheExactLine(t *testing.T) {
	b := Detect(win("10.0.22631.4317", "[wsl2]\nnestedVirtualization=false\n", true).probe())

	for _, w := range []string{"[wsl2]", "nestedVirtualization=true", "wsl --shutdown"} {
		if !strings.Contains(b.Next, w) {
			t.Errorf("next step %q does not contain %q", b.Next, w)
		}
	}
}

func TestOtherSystemsAreRefused(t *testing.T) {
	b := Detect(fakeHost{goos: "freebsd", goarch: "amd64"}.probe())
	if b.Kind != Refused || !strings.Contains(b.Reason, "KVM") {
		t.Fatalf("freebsd = %s %q", b.Kind, b.Reason)
	}
}

func TestKubernetesIsARuntimeClass(t *testing.T) {
	b := ForProvider("kubernetes", fakeHost{goos: "darwin", goarch: "arm64"}.probe())
	if b.Kind != KataRuntimeClass || !strings.Contains(b.Reason, "kata-fc") {
		t.Fatalf("kubernetes = %s %q", b.Kind, b.Reason)
	}

	h := fakeHost{goos: "linux", env: map[string]string{"SBX_KATA_FC_RUNTIMECLASS": "kata-firecracker"}}
	if b := ForProvider("k8s", h.probe()); !strings.Contains(b.Reason, "kata-firecracker") {
		t.Fatalf("configured name ignored: %q", b.Reason)
	}
}

func TestDoctorRow(t *testing.T) {
	ctx := t.Context()
	helper := Backend{Kind: HelperVM, Helper: "lima", Reason: "Apple M4 on macOS 26.4.1: ..."}

	cases := []struct {
		name    string
		b       Backend
		state   State
		err     error
		have    bool
		detail  []string
		meaning string
	}{
		{"direct", Backend{Kind: Direct, Reason: "/dev/kvm is present"}, "", nil, true, []string{"direct", "/dev/kvm"}, ""},
		{"helper, never created", helper, Absent, nil, true, []string{"helper VM (lima)", "sbx-fc not created yet", "first use"}, ""},
		{"helper, stopped", helper, Stopped, nil, true, []string{"sbx-fc stopped", "on demand"}, ""},
		{"helper, running", helper, Running, nil, true, []string{"sbx-fc running"}, ""},
		{"helper, tool broken", helper, "", errors.New("limactl: boom"), false, []string{"limactl: boom"}, "sbx fc vm status"},
		{"refused", Backend{Kind: Refused, Reason: "no /dev/kvm on Amazon EC2 t3.large", Next: "use a .metal instance"}, "", nil, false, []string{"refused", "Amazon EC2"}, ".metal"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			have, detail, meaning := DoctorRow(ctx, c.b, "sbx-fc", func(context.Context) (State, error) { return c.state, c.err })

			if have != c.have {
				t.Errorf("have = %v", have)
			}

			for _, w := range c.detail {
				if !strings.Contains(detail, w) {
					t.Errorf("detail %q lacks %q", detail, w)
				}
			}

			if !strings.Contains(meaning, c.meaning) || (!have && meaning == "") {
				t.Errorf("meaning %q, want it to contain %q", meaning, c.meaning)
			}
		})
	}
}

// Detect and the provider must never disagree about Linux or a Mac: both are hostcap.Decide.
// A /dev/kvm that exists but answers the wrong API version is the case the old presence-only
// check here approved and the provider then refused.
func TestLinuxAndMacAreHostcapsDecision(t *testing.T) {
	h := fakeHost{goos: "linux", goarch: "amd64", devs: map[string]bool{"/dev/kvm": true}}
	p := h.probe()
	p.KVM = func() hostcap.KVM { return hostcap.KVM{Present: true, APIVersion: 11} }

	b := Detect(p)
	if b.Kind != Refused || !strings.Contains(b.Reason, "API version 11") {
		t.Fatalf("a KVM answering API 11 = %s %q, want hostcap's refusal", b.Kind, b.Reason)
	}

	for _, h := range []fakeHost{
		mac("Apple M4", "26.4.1", "limactl"),
		mac("Apple M2", "26.4.1", "limactl"),
		mac("Apple M3", "14.1", "limactl"),
		{goos: "linux", goarch: "arm64", files: map[string]string{"/sys/class/dmi/id/sys_vendor": "Amazon EC2\n"}},
		{goos: "linux", goarch: "arm64", devs: map[string]bool{"/dev/kvm": true}},
	} {
		want := hostcap.Decide(report(h.probe()))
		got := Detect(h.probe())

		if got.Kind != want.Backend || !strings.HasPrefix(got.Reason, want.Reason) || got.Next != want.Next {
			t.Errorf("%s/%s: Detect = %+v, hostcap = %+v", h.goos, h.goarch, got, want)
		}
	}
}
