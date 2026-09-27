package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"sort"
	"strings"
)

// Install is doctor's other half. Doctor reports what is missing and never changes the host;
// this installs what it reported, and only when asked. They are two commands on purpose: a
// report you can run anywhere, any time, without it touching anything, and a change you ask
// for by name, see in full before it runs, and confirm.
//
// Tools come from the host's own package manager. The isolation runtimes and checkpoint are
// more than a package: they are registered with the docker daemon, so those recipes also write
// /etc/docker/daemon.json and reload or restart dockerd (install_runtime.go). Anything fetched
// from outside a package manager is pinned by hash, like the firecracker artifacts are.

// PackageManager is one package manager this knows how to drive.
type PackageManager struct {
	Name    string
	root    bool     // needs root: prefixed with sudo when not already root
	refresh []string // run once before installing, when the index can be stale (apt)
	install []string // the packages are appended
}

// The managers, in the order they are looked for. On Linux the distribution's own comes first:
// a Linuxbrew next to apt should not win over apt for iptables.
var managers = []PackageManager{
	{Name: "apt-get", root: true, refresh: []string{"apt-get", "update"}, install: []string{"apt-get", "install", "-y"}},
	{Name: "dnf", root: true, install: []string{"dnf", "install", "-y"}},
	{Name: "pacman", root: true, install: []string{"pacman", "-S", "--needed", "--noconfirm"}},
	{Name: "apk", root: true, install: []string{"apk", "add"}},
	{Name: "brew", install: []string{"brew", "install"}},
}

// installable maps a doctor row's name to the package that provides it, per manager. A manager
// missing from a row has no package for it in its default repositories; the plan says so rather
// than guessing a name that fails halfway through an install.
var installable = map[string]map[string][]string{
	"docker": {
		"apt-get": {"docker.io"}, "dnf": {"moby-engine"}, "pacman": {"docker"}, "apk": {"docker"},
		// The CLI and a VM to run the daemon in; Docker Desktop is a GUI install and a licence.
		"brew": {"docker", "colima"},
	},
	"kubectl": {
		"dnf": {"kubernetes-client"}, "pacman": {"kubectl"}, "apk": {"kubectl"}, "brew": {"kubectl"},
	},
	"cloudflared": {
		"pacman": {"cloudflared"}, "brew": {"cloudflared"},
	},
	"redis-cli": {
		"apt-get": {"redis-tools"}, "dnf": {"redis"}, "apk": {"redis"}, "brew": {"redis"},
	},
	// The firecracker provider's needs; doctor shows these rows only where Firecracker runs
	// directly, which is Linux.
	"mkfs.ext4": {
		"apt-get": {"e2fsprogs"}, "dnf": {"e2fsprogs"}, "pacman": {"e2fsprogs"}, "apk": {"e2fsprogs"},
	},
	"iptables": {
		"apt-get": {"iptables"}, "dnf": {"iptables"}, "pacman": {"iptables"}, "apk": {"iptables"},
	},
}

// Installable is every name `sbx install` accepts, sorted: the tools, then the runtimes.
func Installable() []string {
	names := make([]string, 0, len(installable)+len(runtimes))
	for n := range installable {
		names = append(names, n)
	}

	sort.Strings(names)

	rts := make([]string, 0, len(runtimes))
	for n := range runtimes {
		rts = append(rts, n)
	}

	sort.Strings(rts)

	return append(names, rts...)
}

// canonical accepts a doctor row's own name for a runtime - "isolation gvisor", "docker
// checkpoint" - as well as the short one, so what doctor printed can be pasted.
func canonical(name string) string {
	for short, r := range runtimes {
		if name == r.row {
			return short
		}
	}

	return name
}

// DetectManager finds the first package manager on PATH. ok is false when there is none this
// knows how to drive - Windows, or a distribution outside the list.
func DetectManager(lookPath func(string) (string, error)) (PackageManager, bool) {
	for _, m := range managers {
		if _, err := lookPath(m.Name); err == nil {
			return m, true
		}
	}

	return PackageManager{}, false
}

// Host is everything a plan depends on, read once, so that planning is a pure function a test
// can hand any machine to.
type Host struct {
	PM      PackageManager
	HasPM   bool
	EUID    int
	HasSudo bool
	Arch    string // GOARCH

	// DaemonElsewhere is empty when the docker daemon is this machine's rootful dockerd - the only
	// one whose runtimes sbx can register - and otherwise says where it is and why that is out of
	// reach.
	DaemonElsewhere string
	KVM             bool
	DaemonJSON      []byte // /etc/docker/daemon.json as it is now; nil when there is none
	DaemonJSONErr   error  // reading it failed for a reason other than its absence

	// NoCandidate reports a package the manager's configured sources do not have, so the plan
	// skips it rather than failing the whole transaction on it. nil when that cannot be asked.
	NoCandidate func(pkg string) bool
}

// ReadHost reads this machine.
func ReadHost() Host {
	pm, ok := DetectManager(exec.LookPath)
	_, sudoErr := exec.LookPath("sudo")
	_, kvmErr := os.Stat("/dev/kvm")

	h := Host{PM: pm, HasPM: ok, EUID: os.Geteuid(), HasSudo: sudoErr == nil, Arch: runtime.GOARCH,
		DaemonElsewhere: daemonElsewhere(), KVM: kvmErr == nil}

	if ok && pm.Name == "apt-get" {
		h.NoCandidate = aptNoCandidate
	}

	if b, err := os.ReadFile(daemonJSONPath); err == nil {
		h.DaemonJSON = b
	} else if !errors.Is(err, os.ErrNotExist) {
		h.DaemonJSONErr = err
	}

	return h
}

// InstallPlan is what would run, and what would not and why - built before anything runs, so
// the whole of it can be shown and confirmed first.
type InstallPlan struct {
	Manager  string
	Packages []string   // deduplicated, in the order first needed
	For      []string   // the names this plan installs
	Skipped  []string   // one line each: a name that is present already, or cannot be done here
	Notes    []string   // what running it will do beyond installing, such as restarting dockerd
	Commands [][]string // exactly what runs, sudo included
}

// Empty says there is nothing to run.
func (p InstallPlan) Empty() bool { return len(p.Commands) == 0 }

// PlanInstall decides what to install for want, given which doctor rows are missing. want is
// what was asked for, or every installable missing name when nothing was named.
func PlanInstall(want []string, missing map[string]bool, h Host) (InstallPlan, error) {
	plan := InstallPlan{Manager: h.PM.Name}
	named := len(want) > 0

	if !named {
		for _, n := range Installable() {
			if missing[rowOf(n)] {
				want = append(want, n)
			}
		}
	}

	var (
		chosen []runtimeRecipe
		skip   = func(format string, a ...any) { plan.Skipped = append(plan.Skipped, fmt.Sprintf(format, a...)) }
	)

	for _, n := range want {
		n = canonical(n)

		byManager, isTool := installable[n]
		r, isRuntime := runtimes[n]

		switch {
		case !isTool && !isRuntime:
			return InstallPlan{}, fmt.Errorf("sbx install does not know how to install %q; it installs %s",
				n, strings.Join(Installable(), ", "))
		case !missing[rowOf(n)]:
			skip("%s: already here", n)

			continue
		case isRuntime:
			if why := r.refuse(h); why != "" {
				skip("%s: %s", n, why)

				continue
			}

			byManager = r.packages
		}

		pkgs, ok := byManager[h.PM.Name]

		switch {
		case len(byManager) > 0 && !h.HasPM:
			skip("%s: no package manager this knows (%s) is on PATH; see its own install instructions", n, managerNames())

			continue
		case len(byManager) > 0 && !ok:
			skip("%s: %s has no package for it; see its own install instructions", n, h.PM.Name)

			continue
		}

		if i := slices.IndexFunc(pkgs, func(p string) bool { return h.NoCandidate != nil && h.NoCandidate(p) }); i >= 0 {
			why := fmt.Sprintf("%s: %s has no %s package in its configured sources", n, h.PM.Name, pkgs[i])
			if isRuntime && r.noPackage != "" {
				why += "; " + r.noPackage
			}

			skip("%s", why)

			continue
		}

		plan.For = append(plan.For, n)

		for _, p := range pkgs {
			if !slices.Contains(plan.Packages, p) {
				plan.Packages = append(plan.Packages, p)
			}
		}

		if isRuntime {
			chosen = append(chosen, r)
		}
	}

	var prefix []string

	if h.EUID != 0 && (len(chosen) > 0 || (len(plan.Packages) > 0 && h.PM.root)) {
		if !h.HasSudo {
			return InstallPlan{}, errors.New("this needs root and sudo is not on PATH; run sbx install as root")
		}

		prefix = []string{"sudo"}
	}

	if len(plan.Packages) > 0 {
		pmPrefix := prefix
		if !h.PM.root {
			pmPrefix = nil
		}

		if h.PM.refresh != nil {
			plan.Commands = append(plan.Commands, append(slices.Clone(pmPrefix), h.PM.refresh...))
		}

		cmd := append(slices.Clone(pmPrefix), h.PM.install...)
		plan.Commands = append(plan.Commands, append(cmd, plan.Packages...))
	}

	if len(chosen) > 0 {
		scripts, notes, err := runtimeSteps(chosen, h)
		if err != nil {
			return InstallPlan{}, err
		}

		for _, s := range scripts {
			plan.Commands = append(plan.Commands, append(slices.Clone(prefix), "sh", "-c", s))
		}

		plan.Notes = notes
	}

	return plan, nil
}

// rowOf is the doctor row a name answers to.
func rowOf(name string) string {
	if r, ok := runtimes[name]; ok {
		return r.row
	}

	return name
}

// MissingFromReport is the set of doctor rows that are absent.
func MissingFromReport(rep Report) map[string]bool {
	missing := map[string]bool{}

	for _, c := range rep.Capabilities {
		if !c.Have {
			missing[c.Name] = true
		}
	}

	return missing
}

// PrintPlan writes the plan for a human to read before saying yes.
func PrintPlan(w io.Writer, p InstallPlan) {
	for _, s := range p.Skipped {
		fmt.Fprintf(w, "  - %s\n", s)
	}

	if p.Empty() {
		fmt.Fprintln(w, "nothing to install")
		return
	}

	fmt.Fprintf(w, "installs %s:\n", strings.Join(p.For, ", "))

	for _, c := range p.Commands {
		// A script is shown as the script, so it can be read line by line rather than as one
		// quoted argument.
		if n := len(c); n >= 3 && c[n-3] == "sh" && c[n-2] == "-c" {
			fmt.Fprintf(w, "  $ %s <<'EOF'\n", strings.Join(c[:n-2], " "))

			for _, line := range strings.Split(strings.TrimRight(c[n-1], "\n"), "\n") {
				fmt.Fprintf(w, "    %s\n", line)
			}

			fmt.Fprintln(w, "  EOF")

			continue
		}

		fmt.Fprintf(w, "  $ %s\n", strings.Join(c, " "))
	}

	for _, n := range p.Notes {
		fmt.Fprintf(w, "note: %s\n", n)
	}
}

// InstallOptions are the flags.
type InstallOptions struct {
	Names  []string
	Yes    bool // run without asking
	DryRun bool // print the plan and stop
}

// errNotConfirmed is returned when the answer was not yes, so the exit status says nothing ran.
var errNotConfirmed = errors.New("nothing installed")

// Install runs doctor, plans, shows the plan, asks, and runs it with the terminal attached -
// sudo may want a password, and the package manager's own progress is the progress. Doctor runs
// again after, so what it says is what the machine now has.
func Install(ctx context.Context, opts InstallOptions, in io.Reader, out io.Writer, interactive bool) error {
	plan, err := PlanInstall(opts.Names, MissingFromReport(Doctor(ctx)), ReadHost())
	if err != nil {
		return err
	}

	PrintPlan(out, plan)

	if plan.Empty() || opts.DryRun {
		return nil
	}

	if !opts.Yes {
		if !interactive {
			return errors.New("not a terminal, so not asking: pass --yes to install, or --dry-run to only see the plan")
		}

		fmt.Fprint(out, "proceed? [y/N] ")

		answer, _ := bufio.NewReader(in).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
			return errNotConfirmed
		}
	}

	for i, c := range plan.Commands {
		cmd := exec.CommandContext(ctx, c[0], c[1:]...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, out, os.Stderr

		if err := cmd.Run(); err != nil {
			// By its place in the plan printed above: a script quoted back whole is unreadable.
			return fmt.Errorf("step %d of %d failed (%w); the steps before it are done, and running "+
				"sbx install again picks up from what doctor then reports", i+1, len(plan.Commands), err)
		}
	}

	fmt.Fprintln(out)

	return PrintReport(out, Doctor(ctx), false)
}

// aptNoCandidate asks apt whether pkg can be installed from the sources it has.
func aptNoCandidate(pkg string) bool {
	out, err := exec.Command("apt-cache", "policy", pkg).Output()
	if err != nil {
		return false // cannot tell; let apt-get say
	}

	return !strings.Contains(string(out), "Candidate:") || strings.Contains(string(out), "Candidate: (none)")
}

func managerNames() string {
	names := make([]string, len(managers))
	for i, m := range managers {
		names[i] = m.Name
	}

	return strings.Join(names, ", ")
}

// installHint is the line doctor ends on when something it reports missing can be installed
// here. It is what `sbx install` with no names would plan, so it never promises what install
// would then skip.
func installHint(rep Report, h Host) string {
	plan, err := PlanInstall(nil, MissingFromReport(rep), h)
	if err != nil || plan.Empty() {
		return ""
	}

	return "missing and installable here: " + strings.Join(plan.For, ", ") +
		" - `sbx install` shows the commands and asks first"
}
