package cli

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/fc"
	"github.com/aryanmehrotra/sbx/internal/fc/hostcap"
)

func manager(t *testing.T, name string) PackageManager {
	t.Helper()

	for _, m := range managers {
		if m.Name == name {
			return m
		}
	}

	t.Fatalf("no manager %q", name)

	return PackageManager{}
}

// host is a machine with the named manager, running as euid; the daemon is local and there is
// no daemon.json yet.
func host(t *testing.T, pm string, euid int, sudo bool) Host {
	t.Helper()

	return Host{PM: manager(t, pm), HasPM: true, EUID: euid, HasSudo: sudo, Arch: "amd64", KVM: true}
}

// With no names, install covers exactly what doctor reported missing - not what is present, and
// not rows it has no package for (the docker daemon is not a package).
func TestInstallPlansOnlyWhatIsMissing(t *testing.T) {
	missing := map[string]bool{"redis-cli": true, "kubectl": true, "docker daemon": true}

	p, err := PlanInstall(nil, missing, host(t, "brew", 501, true))
	if err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(p.For, []string{"kubectl", "redis-cli"}) {
		t.Errorf("planned for %v, want kubectl and redis-cli", p.For)
	}

	want := [][]string{{"brew", "install", "kubectl", "redis"}}
	if !slices.EqualFunc(p.Commands, want, slices.Equal) {
		t.Errorf("commands %v, want %v", p.Commands, want)
	}
}

// A root-only manager gets sudo when not root, and not when root; apt refreshes its index first.
func TestInstallUsesSudoOnlyWhenNeeded(t *testing.T) {
	missing := map[string]bool{"iptables": true}

	p, err := PlanInstall([]string{"iptables"}, missing, host(t, "apt-get", 1000, true))
	if err != nil {
		t.Fatal(err)
	}

	want := [][]string{{"sudo", "apt-get", "update"}, {"sudo", "apt-get", "install", "-y", "iptables"}}
	if !slices.EqualFunc(p.Commands, want, slices.Equal) {
		t.Errorf("as a user: %v, want %v", p.Commands, want)
	}

	p, err = PlanInstall([]string{"iptables"}, missing, host(t, "apt-get", 0, false))
	if err != nil {
		t.Fatal(err)
	}

	if p.Commands[1][0] != "apt-get" {
		t.Errorf("as root it still prefixed something: %v", p.Commands)
	}

	if _, err := PlanInstall([]string{"iptables"}, missing, host(t, "apt-get", 1000, false)); err == nil {
		t.Error("a user without sudo was given a plan that cannot run")
	}
}

// Asked for something present, or something this manager has no package for, it says so and
// runs nothing - rather than guessing a package name that fails halfway through.
func TestInstallExplainsWhatItSkips(t *testing.T) {
	missing := map[string]bool{"kubectl": true}

	p, err := PlanInstall([]string{"kubectl", "redis-cli"}, missing, host(t, "apt-get", 0, false))
	if err != nil {
		t.Fatal(err)
	}

	if !p.Empty() {
		t.Errorf("apt has no kubectl and redis-cli is present, but it planned %v", p.Commands)
	}

	joined := strings.Join(p.Skipped, "\n")
	for _, want := range []string{"redis-cli: already here", "kubectl: apt-get has no package"} {
		if !strings.Contains(joined, want) {
			t.Errorf("skipped lines do not say %q:\n%s", want, joined)
		}
	}
}

func TestInstallRefusesAnUnknownName(t *testing.T) {
	_, err := PlanInstall([]string{"runc"}, map[string]bool{"runc": true}, host(t, "brew", 0, false))
	if err == nil || !strings.Contains(err.Error(), "redis-cli") {
		t.Errorf("an unknown name should be refused with the list of known ones, got %v", err)
	}
}

// A package two rows share is installed once.
func TestInstallDeduplicatesPackages(t *testing.T) {
	installable["test-a"] = map[string][]string{"brew": {"shared", "a"}}
	installable["test-b"] = map[string][]string{"brew": {"shared"}}

	t.Cleanup(func() { delete(installable, "test-a"); delete(installable, "test-b") })

	p, err := PlanInstall([]string{"test-a", "test-b"}, map[string]bool{"test-a": true, "test-b": true},
		host(t, "brew", 0, false))
	if err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(p.Packages, []string{"shared", "a"}) {
		t.Errorf("packages %v", p.Packages)
	}
}

func TestDetectManagerPrefersTheDistributions(t *testing.T) {
	onPath := func(names ...string) func(string) (string, error) {
		return func(n string) (string, error) {
			if slices.Contains(names, n) {
				return "/usr/bin/" + n, nil
			}

			return "", errors.New("not found")
		}
	}

	if m, ok := DetectManager(onPath("brew", "apt-get")); !ok || m.Name != "apt-get" {
		t.Errorf("with brew and apt-get, got %q", m.Name)
	}

	if _, ok := DetectManager(onPath("winget")); ok {
		t.Error("found a manager it cannot drive")
	}
}

// Every installable name must be a row doctor can actually report, or install offers to fix
// something doctor never calls broken.
func TestEveryInstallableNameIsADoctorRow(t *testing.T) {
	rows := map[string]bool{"docker": true, "kubectl": true, "cloudflared": true, "redis-cli": true,
		"isolation gvisor": true, "isolation kata": true, "docker checkpoint": true}
	for _, c := range firecrackerCapabilities(hostcap.Direct, "", fc.BridgeIsolation{}, nil) {
		rows[c.Name] = true
	}

	for _, c := range firecrackerGuardRows(hostcap.Direct, errors.New("x"), fc.GuardCount{}, nil) {
		rows[c.Name] = true
	}

	for _, n := range Installable() {
		if !rows[rowOf(n)] {
			t.Errorf("%q is installable but is not a doctor row", n)
		}
	}
}

// Doctor points at install only for what `sbx install` would do here, and stays quiet otherwise
// - a hint that install then skips is a promise broken on the next command.
func TestDoctorPointsAtInstall(t *testing.T) {
	rep := Report{Host: "x", Capabilities: []Capability{
		{Name: "redis-cli", Detail: "not on PATH", Meaning: "m"},
		{Name: "kubectl", Detail: "not on PATH", Meaning: "m"},
	}}

	if h := installHint(rep, host(t, "apt-get", 0, false)); h != "missing and installable here: redis-cli - `sbx install` shows the commands and asks first" {
		t.Errorf("hint: %q", h)
	}

	if h := installHint(rep, Host{EUID: 0}); h != "" {
		t.Errorf("no manager, yet a hint: %q", h)
	}

	rep.Capabilities[0].Have = true
	if h := installHint(rep, host(t, "apt-get", 0, false)); h != "" {
		t.Errorf("nothing apt can install is missing, yet a hint: %q", h)
	}
}
