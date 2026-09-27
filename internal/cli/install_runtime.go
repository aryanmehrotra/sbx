package cli

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strings"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// The runtimes `sbx install` can add to the docker daemon: what doctor reports under
// "isolation gvisor", "isolation kata" and "docker checkpoint".
//
// Each is registered in /etc/docker/daemon.json under the name the docker provider asks for
// (dockerRuntime: runsc, kata-runtime), because the daemon, not a binary on PATH, is what doctor
// and `--isolation` consult. So these are done only against this machine's rootful dockerd.
// Where the daemon runs in a VM (Docker Desktop, colima, Rancher Desktop) its config belongs to
// that VM's manager, which rewrites it on the next start; an edit made from here would work until
// then and vanish, which is worse than saying so.

const daemonJSONPath = "/etc/docker/daemon.json"

// gVisor ships as one tarball per release: runsc, its containerd shim, and gvisor-bin/, the
// sidecars runsc looks for next to itself - so it is unpacked whole into a directory of its own
// and registered by that path, not copied into /usr/local/bin. Pinned by sha512, the digest the
// release publishes beside the tarball.
const (
	gvisorRelease = "20260921.0"
	gvisorDir     = "/usr/local/lib/sbx/gvisor/release-" + gvisorRelease
	gvisorBase    = "https://storage.googleapis.com/gvisor/releases/release/" + gvisorRelease
)

var gvisorSHA512 = map[string]string{
	"amd64": "7c899979bed334f0987888c41e545cede8e1f7e67a257978c8de244c3867e32bc30d08491e9af36720273c502acd01d8d86c9844ebcbe125790c43fe6bdf563e",
	"arm64": "9438f926b8fadee8c0b8c5c2c954cfc957618acd5d9e1d669e442a9366412dd8794e210b9ce8c7a880a49c8ee9f4f040943a11edae00893d2b22ca844c61de93",
}

// gvisorArch is the release's directory name for a GOARCH.
var gvisorArch = map[string]string{"amd64": "x86_64", "arm64": "aarch64"}

type runtimeRecipe struct {
	row      string              // the doctor row it fixes
	packages map[string][]string // from the host's package manager, per manager; nil for none
	fetch    func(arch string) string
	key      func(cfg map[string]any) bool // already in daemon.json
	set      func(cfg map[string]any, arch string)
	restart  bool // dockerd must restart to see it; runtimes alone need only a reload
	// noPackage is where to get the package from when the manager's sources lack it.
	noPackage string
	needsKVM  bool
}

var runtimes = map[string]runtimeRecipe{
	"gvisor": {
		row: "isolation gvisor",
		fetch: func(arch string) string {
			url := gvisorBase + "/" + gvisorArch[arch] + "/gvisor.tar.bz2"

			return strings.Join([]string{
				"set -eu",
				"for t in curl bzip2 sha512sum tar; do command -v $t >/dev/null || { echo \"sbx install: gvisor needs $t\" >&2; exit 1; }; done",
				"tmp=$(mktemp -d); trap 'rm -rf \"$tmp\"' EXIT",
				"curl -fsSL -o \"$tmp/gvisor.tar.bz2\" " + url,
				"echo \"" + gvisorSHA512[arch] + "  $tmp/gvisor.tar.bz2\" | sha512sum -c -",
				"rm -rf " + gvisorDir + ".new && mkdir -p " + gvisorDir + ".new",
				"tar -xjf \"$tmp/gvisor.tar.bz2\" -C " + gvisorDir + ".new",
				"rm -rf " + gvisorDir + " && mv " + gvisorDir + ".new " + gvisorDir,
			}, "\n")
		},
		key: func(cfg map[string]any) bool { return hasRuntimeKey(cfg, "runsc") },
		set: func(cfg map[string]any, _ string) {
			setRuntime(cfg, "runsc", map[string]any{"path": gvisorDir + "/runsc"})
		},
	},
	"kata": {
		row: "isolation kata",
		// Only where the distribution packages it. Upstream's static release is on GitHub, not
		// pinned here yet; kata's own docs cover every other host.
		packages: map[string][]string{"dnf": {"kata-containers"}},
		key:      func(cfg map[string]any) bool { return hasRuntimeKey(cfg, "kata-runtime") },
		set: func(cfg map[string]any, _ string) {
			setRuntime(cfg, "kata-runtime", map[string]any{"runtimeType": "io.containerd.kata.v2"})
		},
		needsKVM: true,
	},
	"checkpoint": {
		row:      "docker checkpoint",
		packages: map[string][]string{"apt-get": {"criu"}, "dnf": {"criu"}, "pacman": {"criu"}},
		key:      func(cfg map[string]any) bool { return cfg["experimental"] == true },
		set:      func(cfg map[string]any, _ string) { cfg["experimental"] = true },
		restart:  true,
		// Ubuntu dropped criu from its archive in 24.04; CRIU's own PPA carries it.
		noPackage: "on Ubuntu add CRIU's PPA (sudo add-apt-repository ppa:criu/ppa) and run this again",
	},
}

// refuse says why r cannot be installed on h, or "" when it can.
func (r runtimeRecipe) refuse(h Host) string {
	switch {
	case h.DaemonElsewhere != "":
		return h.DaemonElsewhere
	case r.fetch != nil && gvisorSHA512[h.Arch] == "":
		return "no release is pinned for " + h.Arch
	case r.needsKVM && !h.KVM:
		return "needs /dev/kvm, which this machine does not have"
	case h.DaemonJSONErr != nil:
		return "cannot read " + daemonJSONPath + " (" + h.DaemonJSONErr.Error() + "); run `sudo sbx install`"
	}

	if cfg, err := parseDaemonJSON(h.DaemonJSON); err == nil && r.key(cfg) {
		return "already in " + daemonJSONPath + " but the daemon does not report it; is dockerd running, and was it restarted?"
	}

	return ""
}

// daemonElsewhere is Host.DaemonElsewhere for this machine.
func daemonElsewhere() string {
	network, address, name, err := provider.DockerSocket()

	if runtime.GOOS != "linux" {
		return "the docker daemon runs inside " + name + "'s VM; register the runtime in that VM's docker config"
	}

	if err != nil {
		if ok, _ := have("dockerd"); ok {
			return ""
		}

		return "no docker daemon on this machine; install docker first (sbx install docker)"
	}

	if network == "unix" && (address == "/var/run/docker.sock" || address == "/run/docker.sock") {
		return ""
	}

	return fmt.Sprintf("the docker daemon at %s://%s is not this machine's rootful dockerd; register the runtime "+
		"in its own config", network, address)
}

// runtimeSteps are the scripts that install rs: fetches, then one daemon.json write for all of
// them, then one reload or restart.
func runtimeSteps(rs []runtimeRecipe, h Host) (scripts, notes []string, err error) {
	cfg, err := parseDaemonJSON(h.DaemonJSON)
	if err != nil {
		return nil, nil, fmt.Errorf("%s is not JSON sbx can merge into, so it is left alone: %w", daemonJSONPath, err)
	}

	restart := false

	for _, r := range rs {
		if r.fetch != nil {
			scripts = append(scripts, r.fetch(h.Arch))
		}

		r.set(cfg, h.Arch)
		restart = restart || r.restart
	}

	body, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, nil, err
	}

	scripts = append(scripts, strings.Join([]string{
		"set -eu",
		"mkdir -p /etc/docker",
		"[ ! -e " + daemonJSONPath + " ] || cp -p " + daemonJSONPath + " " + daemonJSONPath + ".sbx-bak",
		"cat > " + daemonJSONPath + ".sbx-new <<'SBX_JSON'",
		string(body),
		"SBX_JSON",
		"mv " + daemonJSONPath + ".sbx-new " + daemonJSONPath,
	}, "\n"))

	if h.DaemonJSON != nil {
		notes = append(notes, "the previous "+daemonJSONPath+" is kept as "+daemonJSONPath+".sbx-bak")
	}

	// A reload (SIGHUP) picks up runtimes without touching a running container; experimental is
	// read only at start. If dockerd is not running it reads the file when it next starts.
	if restart {
		scripts = append(scripts, "if pgrep -x dockerd >/dev/null; then systemctl restart docker 2>/dev/null || service docker restart "+
			"|| { echo 'sbx install: restart dockerd yourself for this to take effect' >&2; exit 1; }; fi")
		notes = append(notes, "restarts dockerd: running containers stop unless live-restore is on in daemon.json")
	} else {
		scripts = append(scripts, "if pgrep -x dockerd >/dev/null; then systemctl reload docker 2>/dev/null || pkill -HUP -x dockerd; fi")
	}

	return scripts, notes, nil
}

func parseDaemonJSON(b []byte) (map[string]any, error) {
	cfg := map[string]any{}
	if len(strings.TrimSpace(string(b))) == 0 {
		return cfg, nil
	}

	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}

func hasRuntimeKey(cfg map[string]any, name string) bool {
	rts, _ := cfg["runtimes"].(map[string]any)
	_, ok := rts[name]

	return ok
}

func setRuntime(cfg map[string]any, name string, v map[string]any) {
	rts, _ := cfg["runtimes"].(map[string]any)
	if rts == nil {
		rts = map[string]any{}
	}

	rts[name] = v
	cfg["runtimes"] = rts
}
