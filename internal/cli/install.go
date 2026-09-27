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
// It installs through the host's own package manager and nothing else - no curl | sh, no
// binaries fetched from a URL sbx chose. What the package manager installs is what the
// machine's owner already trusts, and it can be removed the same way.

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

// Installable is every name `sbx install` accepts, sorted.
func Installable() []string {
	names := make([]string, 0, len(installable))
	for n := range installable {
		names = append(names, n)
	}

	sort.Strings(names)

	return names
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

// InstallPlan is what would run, and what would not and why - built before anything runs, so
// the whole of it can be shown and confirmed first.
type InstallPlan struct {
	Manager  string
	Packages []string   // deduplicated, in the order first needed
	For      []string   // the doctor rows those packages cover
	Skipped  []string   // one line each: a name that is present already, or has no package here
	Commands [][]string // exactly what runs, sudo included
}

// Empty says there is nothing to run.
func (p InstallPlan) Empty() bool { return len(p.Commands) == 0 }

// PlanInstall decides what to install for want, given which of those names are missing.
// missing is doctor's view; want is what was asked for, or every installable missing name when
// nothing was named. euid decides whether a root-only manager needs sudo in front.
func PlanInstall(want []string, missing map[string]bool, pm PackageManager, euid int, hasSudo bool) (InstallPlan, error) {
	plan := InstallPlan{Manager: pm.Name}

	if len(want) == 0 {
		for _, n := range Installable() {
			if missing[n] {
				want = append(want, n)
			}
		}
	}

	for _, n := range want {
		byManager, known := installable[n]
		if !known {
			return InstallPlan{}, fmt.Errorf("sbx install does not know how to install %q; it installs %s",
				n, strings.Join(Installable(), ", "))
		}

		if !missing[n] {
			plan.Skipped = append(plan.Skipped, n+": already here")
			continue
		}

		pkgs, ok := byManager[pm.Name]
		if !ok {
			plan.Skipped = append(plan.Skipped, fmt.Sprintf("%s: %s has no package for it; see its own install instructions", n, pm.Name))
			continue
		}

		plan.For = append(plan.For, n)

		for _, p := range pkgs {
			if !slices.Contains(plan.Packages, p) {
				plan.Packages = append(plan.Packages, p)
			}
		}
	}

	if len(plan.Packages) == 0 {
		return plan, nil
	}

	var prefix []string

	if pm.root && euid != 0 {
		if !hasSudo {
			return InstallPlan{}, fmt.Errorf("%s needs root and sudo is not on PATH; run sbx install as root", pm.Name)
		}

		prefix = []string{"sudo"}
	}

	if pm.refresh != nil {
		plan.Commands = append(plan.Commands, append(slices.Clone(prefix), pm.refresh...))
	}

	cmd := append(slices.Clone(prefix), pm.install...)
	plan.Commands = append(plan.Commands, append(cmd, plan.Packages...))

	return plan, nil
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

	fmt.Fprintf(w, "installs %s with %s:\n", strings.Join(p.For, ", "), p.Manager)

	for _, c := range p.Commands {
		fmt.Fprintf(w, "  $ %s\n", strings.Join(c, " "))
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

// Install runs doctor, plans, shows the plan, asks, and runs it through the package manager with
// the terminal attached - sudo may want a password, and the manager's own progress is the
// progress. Doctor runs again after, so what it says is what the machine now has.
func Install(ctx context.Context, opts InstallOptions, in io.Reader, out io.Writer, interactive bool) error {
	pm, ok := DetectManager(exec.LookPath)
	if !ok {
		return fmt.Errorf("no supported package manager on %s (looked for %s); install what `sbx doctor` "+
			"reports missing by hand", runtime.GOOS, managerNames())
	}

	_, sudoErr := exec.LookPath("sudo")

	plan, err := PlanInstall(opts.Names, MissingFromReport(Doctor(ctx)), pm, os.Geteuid(), sudoErr == nil)
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

	for _, c := range plan.Commands {
		cmd := exec.CommandContext(ctx, c[0], c[1:]...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, out, os.Stderr

		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s: %w", strings.Join(c, " "), err)
		}
	}

	fmt.Fprintln(out)

	return PrintReport(out, Doctor(ctx), false)
}

func managerNames() string {
	names := make([]string, len(managers))
	for i, m := range managers {
		names[i] = m.Name
	}

	return strings.Join(names, ", ")
}

// installHint is the line doctor ends on when something it reports missing can be installed
// here - by this machine's package manager, so it never promises what `sbx install` would skip.
func installHint(rep Report, pm PackageManager, ok bool) string {
	if !ok {
		return ""
	}

	missing := MissingFromReport(rep)

	var names []string

	for _, n := range Installable() {
		if _, has := installable[n][pm.Name]; missing[n] && has {
			names = append(names, n)
		}
	}

	if len(names) == 0 {
		return ""
	}

	return "missing and installable with " + pm.Name + ": " + strings.Join(names, ", ") +
		" - `sbx install` shows the commands and asks first"
}
