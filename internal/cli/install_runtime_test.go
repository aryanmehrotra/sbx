package cli

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// daemonJSONIn pulls the JSON the plan would write out of its heredoc.
func daemonJSONIn(t *testing.T, p InstallPlan) map[string]any {
	t.Helper()

	for _, c := range p.Commands {
		s := c[len(c)-1]

		start := strings.Index(s, "<<'SBX_JSON'\n")
		if start < 0 {
			continue
		}

		body := s[start+len("<<'SBX_JSON'\n") : strings.Index(s, "\nSBX_JSON")]

		var cfg map[string]any
		if err := json.Unmarshal([]byte(body), &cfg); err != nil {
			t.Fatalf("the daemon.json it writes is not JSON: %v\n%s", err, body)
		}

		return cfg
	}

	t.Fatalf("the plan writes no daemon.json: %v", p.Commands)

	return nil
}

// gVisor: the pinned tarball, checked by its sha512, unpacked whole, and registered under the name
// the docker provider asks for - merged into what daemon.json already says, not over it.
func TestInstallGVisorRegistersRunscAndKeepsTheRest(t *testing.T) {
	h := host(t, "apt-get", 1000, true)
	h.DaemonJSON = []byte(`{"log-driver": "journald", "runtimes": {"nvidia": {"path": "nvidia-container-runtime"}}}`)

	p, err := PlanInstall([]string{"gvisor"}, map[string]bool{"isolation gvisor": true}, h)
	if err != nil {
		t.Fatal(err)
	}

	all := ""
	for _, c := range p.Commands {
		if c[0] != "sudo" {
			t.Errorf("not root, yet %v runs without sudo", c)
		}

		all += c[len(c)-1] + "\n"
	}

	for _, want := range []string{gvisorSHA512["amd64"], "/x86_64/gvisor.tar.bz2", "sha512sum -c", "pkill -HUP -x dockerd"} {
		if !strings.Contains(all, want) {
			t.Errorf("the plan never mentions %q:\n%s", want, all)
		}
	}

	if strings.Contains(all, "restart") {
		t.Error("a runtime needs only a reload, yet the plan restarts dockerd and stops every container")
	}

	cfg := daemonJSONIn(t, p)
	rts := cfg["runtimes"].(map[string]any)

	if rts["runsc"].(map[string]any)["path"] != gvisorDir+"/runsc" {
		t.Errorf("runsc registered as %v", rts["runsc"])
	}

	if rts["nvidia"] == nil || cfg["log-driver"] != "journald" {
		t.Errorf("the merge dropped what was there: %v", cfg)
	}
}

// Doctor's own row names are accepted, so what it printed can be pasted.
func TestInstallAcceptsDoctorRowNames(t *testing.T) {
	p, err := PlanInstall([]string{"isolation gvisor"}, map[string]bool{"isolation gvisor": true}, host(t, "apt-get", 0, false))
	if err != nil || len(p.For) != 1 || p.For[0] != "gvisor" {
		t.Errorf("for %v, err %v", p.For, err)
	}
}

// Checkpoint needs CRIU from the package manager and experimental on - which dockerd reads only at
// start, so this one restarts it, and the plan says what that costs.
func TestInstallCheckpointRestartsAndSaysSo(t *testing.T) {
	p, err := PlanInstall([]string{"checkpoint"}, map[string]bool{"docker checkpoint": true}, host(t, "apt-get", 0, false))
	if err != nil {
		t.Fatal(err)
	}

	if len(p.Packages) != 1 || p.Packages[0] != "criu" {
		t.Errorf("packages %v, want criu", p.Packages)
	}

	// The CRIU check comes before the restart, which can fail (no systemd) and would hide it.
	check, restart := -1, -1

	for i, c := range p.Commands {
		switch s := c[len(c)-1]; {
		case strings.HasPrefix(s, "criu check"):
			check = i
		case strings.Contains(s, "systemctl restart docker"):
			restart = i
		}
	}

	if check < 0 || check > restart {
		t.Errorf("CRIU check at step %d, restart at %d: the check must run, and before the restart", check, restart)
	}

	if daemonJSONIn(t, p)["experimental"] != true {
		t.Error("experimental is not turned on")
	}

	if !strings.Contains(strings.Join(p.Notes, "\n"), "running containers stop") {
		t.Errorf("it restarts dockerd without saying so: %v", p.Notes)
	}
}

// Everything that makes a runtime impossible here is said, not attempted.
func TestInstallRuntimeRefusals(t *testing.T) {
	missing := map[string]bool{"isolation gvisor": true, "isolation kata": true, "docker checkpoint": true}

	for _, tc := range []struct {
		name, want string
		change     func(*Host)
		pm         string
	}{
		{"gvisor", "Docker Desktop's VM", func(h *Host) { h.DaemonElsewhere = "runs inside Docker Desktop's VM" }, "apt-get"},
		{"gvisor", "no release is pinned for riscv64", func(h *Host) { h.Arch = "riscv64" }, "apt-get"},
		{"kata", "needs /dev/kvm", func(h *Host) { h.KVM = false }, "dnf"},
		{"kata", "apt-get has no package", func(*Host) {}, "apt-get"},
		{"gvisor", "run `sudo sbx install`", func(h *Host) { h.DaemonJSONErr = errors.New("permission denied") }, "apt-get"},
		{"gvisor", "does not report it", func(h *Host) { h.DaemonJSON = []byte(`{"runtimes":{"runsc":{}}}`) }, "apt-get"},
	} {
		h := host(t, tc.pm, 0, false)
		tc.change(&h)

		p, err := PlanInstall([]string{tc.name}, missing, h)
		if err != nil {
			t.Fatal(err)
		}

		if !p.Empty() || !strings.Contains(strings.Join(p.Skipped, "\n"), tc.want) {
			t.Errorf("%s: want a skip saying %q, got skipped %v, commands %v", tc.name, tc.want, p.Skipped, p.Commands)
		}
	}
}

// A daemon.json that is not JSON is someone's config sbx does not understand; it is not replaced.
func TestInstallLeavesABrokenDaemonJSONAlone(t *testing.T) {
	h := host(t, "apt-get", 0, false)
	h.DaemonJSON = []byte("{ not json")

	if _, err := PlanInstall([]string{"gvisor"}, map[string]bool{"isolation gvisor": true}, h); err == nil {
		t.Error("planned to overwrite a daemon.json it could not parse")
	}
}

// Two runtimes in one run: one daemon.json write with both, one restart.
func TestInstallRuntimesShareOneWrite(t *testing.T) {
	missing := map[string]bool{"isolation gvisor": true, "isolation kata": true, "docker checkpoint": true}

	p, err := PlanInstall(nil, missing, host(t, "dnf", 0, false))
	if err != nil {
		t.Fatal(err)
	}

	writes := 0
	for _, c := range p.Commands {
		writes += strings.Count(c[len(c)-1], "SBX_JSON\nmv")
	}

	cfg := daemonJSONIn(t, p)
	rts := cfg["runtimes"].(map[string]any)

	if writes != 1 || rts["runsc"] == nil || rts["kata-runtime"] == nil || cfg["experimental"] != true {
		t.Errorf("%d writes; config %v", writes, cfg)
	}
}

// A package the sources do not have is skipped with where to get it, rather than planned into an
// apt transaction that then fails and takes every other package in it down too.
func TestInstallSkipsAPackageTheSourcesLack(t *testing.T) {
	h := host(t, "apt-get", 0, false)
	h.NoCandidate = func(p string) bool { return p == "criu" }

	p, err := PlanInstall(nil, map[string]bool{"docker checkpoint": true, "redis-cli": true}, h)
	if err != nil {
		t.Fatal(err)
	}

	if len(p.Packages) != 1 || p.Packages[0] != "redis-tools" {
		t.Errorf("packages %v, want only redis-tools", p.Packages)
	}

	if !strings.Contains(strings.Join(p.Skipped, "\n"), "ppa:criu/ppa") {
		t.Errorf("the skip does not say where criu comes from: %v", p.Skipped)
	}
}
