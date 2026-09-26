package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"

	"github.com/aryanmehrotra/sbx/internal/fc"
	"github.com/aryanmehrotra/sbx/internal/fchost"
	"github.com/aryanmehrotra/sbx/internal/hostinfo"
	"github.com/aryanmehrotra/sbx/internal/provider"
	"slices"
	"strings"

	"github.com/aryanmehrotra/sbx/internal/daemon"
)

// Doctor answers the question you have before you trust a sandbox with anything: what can
// this machine actually do?
//
// sbx already refuses rather than silently downgrades - asking for gVisor on a host without
// it fails, and it says why. That is the right behaviour and it is also the wrong moment to
// find out. Everything here is a capability someone reads about in the docs and then has to
// discover by trying, which is a bad way to learn that your isolation tier is not available.
//
// It states what is missing and what that costs. It never installs anything: a tool that
// silently changes a host to make its own claims true is worse than one that reports the
// truth.

// Capability is one thing the host either can or cannot do.
type Capability struct {
	Name    string `json:"name"`
	Have    bool   `json:"have"`
	Detail  string `json:"detail"`
	Meaning string `json:"meaning,omitempty"` // what its absence costs, when it is absent
}

// Report is the whole answer, in a shape a script can read.
type Report struct {
	Host         string       `json:"host"`
	Capabilities []Capability `json:"capabilities"`
}

func have(name string) (bool, string) {
	path, err := exec.LookPath(name)
	if err != nil {
		return false, "not on PATH"
	}

	return true, path
}

// dockerInfo reads one field from the daemon. Separate from the binary check because a
// docker CLI with no daemon behind it is a different failure from no docker at all, and
// they need different advice.
func dockerInfo(ctx context.Context, format string) (string, bool) {
	out, err := exec.CommandContext(ctx, "docker", "info", "--format", format).Output()
	if err != nil {
		return "", false
	}

	return strings.TrimSpace(string(out)), true
}

// dockerRuntimes lists the runtimes the daemon has registered. This is the authority on
// whether `--isolation gvisor` can work, rather than whether a runsc binary happens to sit
// on this PATH: the daemon runs the container, and on macOS the daemon is in a VM where
// this PATH means nothing.
func dockerRuntimes(ctx context.Context) []string {
	out, ok := dockerInfo(ctx, "{{json .Runtimes}}")
	if !ok {
		return nil
	}

	var m map[string]any
	if json.Unmarshal([]byte(out), &m) != nil {
		return nil
	}

	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}

	return names
}

func hasRuntime(runtimes []string, want string) bool {
	return slices.Contains(runtimes, want)
}

// Doctor collects the report. It takes no provider: the point is to run before anything is
// created, including on a machine where nothing works at all.
func Doctor(ctx context.Context) Report {
	rep := Report{Host: runtime.GOOS + "/" + runtime.GOARCH}

	// What this machine has, before what it can do with it. A sandbox per branch is a question
	// about room, and the answer is different on a laptop with 8 GB than on one with 64 - so
	// the first thing the report says is how much there is.
	//
	// Where the figures cannot be read - anything but macOS and Linux - the entry says so
	// rather than being left out. Absent and unsupported are different, and only one of them
	// is worth going and looking into.
	rep.Capabilities = append(rep.Capabilities, machineRow(runtime.GOOS, hostinfo.Read()))

	dockerOK, dockerWhere := have("docker")
	rep.Capabilities = append(rep.Capabilities, Capability{
		Name: "docker", Have: dockerOK, Detail: dockerWhere,
		Meaning: "the default provider; without it use --provider kubernetes",
	})

	if dockerOK {
		ver, up := dockerInfo(ctx, "{{.ServerVersion}}")
		detail := ver

		meaning := "nothing can be created until the daemon is running"

		if !up {
			// Which runtime, and the command that starts it. "the daemon did not answer" is
			// true of a machine where colima was stopped, where Docker Desktop was never
			// opened and where the VM is wedged, and those want different things done.
			name, start := provider.RuntimeHint()

			detail = "the CLI is here but the daemon did not answer"
			meaning = fmt.Sprintf("nothing can be created until it is running - %s looks like "+
				"the runtime here, so try `%s`", name, start)
		}

		rep.Capabilities = append(rep.Capabilities, Capability{
			Name: "docker daemon", Have: up, Detail: detail, Meaning: meaning,
		})
	}

	// The one capability whose absence breaks everything, and it was the one doctor did not
	// report. README says the daemon "owns the ports sbx env hands out, so nothing works
	// without it", and then doctor listed a missing redis-cli that only affects selftest
	// while saying nothing about this. A daemon started with `sbx serve &` dies with the
	// terminal, and the first thing anyone runs afterwards is doctor.
	rep.Capabilities = append(rep.Capabilities, daemonCapability())

	rts := dockerRuntimes(ctx)

	for _, iso := range []struct{ flag, runtime, why string }{
		{"isolation gvisor", "runsc", "--isolation gvisor is refused; a container shares the host kernel"},
		{"isolation kata", "kata-runtime", "--isolation kata is refused; a container shares the host kernel"},
	} {
		ok := hasRuntime(rts, iso.runtime)
		detail := "runtime " + iso.runtime + " not registered with the docker daemon"

		if ok {
			detail = "runtime " + iso.runtime + " available"
		}

		rep.Capabilities = append(rep.Capabilities, Capability{
			Name: iso.flag, Have: ok, Detail: detail, Meaning: iso.why,
		})
	}

	// A microVM is a backend decision, not a docker runtime, so it gets its own row: which
	// backend `--provider firecracker` would use from here and why - directly, through a helper
	// VM, or refused with the fix. It only reads; the helper VM is never started from here.
	// One decision (fchost.HostBackend) feeds the row and the detail rows under it, and is the
	// same one the provider and the redirect act on.
	fb := fchost.HostBackend()
	fcHave, fcDetail, fcMeaning := fchost.HostDoctorRow(ctx, fb)
	rep.Capabilities = append(rep.Capabilities, Capability{
		Name: "microVM", Have: fcHave, Detail: fcDetail, Meaning: fcMeaning,
	})

	mkfsPath := ""
	if ok, where := have("mkfs.ext4"); ok {
		mkfsPath = where
	}

	var bridges fc.BridgeIsolation
	if fb.Kind == fchost.Direct {
		bridges = fc.HostBridgeIsolation()
	}

	var usage *provider.FirecrackerUsage
	if u, err := provider.FirecrackerDiskUsage(); err == nil {
		usage = &u
	}

	rep.Capabilities = append(rep.Capabilities, firecrackerCapabilities(fb.Kind, mkfsPath, bridges, usage)...)

	if fb.Kind == fchost.Direct {
		iptErr := fc.Available()

		var (
			guards   fc.GuardCount
			countErr error
		)

		if iptErr == nil {
			guards, countErr = fc.CountGuards(ctx, fc.NewGuard(provider.EgressProxyPort), fc.BridgeSlots())
		}

		rep.Capabilities = append(rep.Capabilities, firecrackerGuardRows(fb.Kind, iptErr, guards, countErr)...)
		rep.Capabilities = append(rep.Capabilities, firecrackerGuards(os.Getenv, fc.Available, fc.Cgroup2)...)
	}

	// Checkpoint/restore, which is what a memory-preserving sleep would need. Two things
	// have to be true and they fail differently, so both are reported.
	exp, _ := dockerInfo(ctx, "{{.ExperimentalBuild}}")
	rep.Capabilities = append(rep.Capabilities, Capability{
		Name: "docker checkpoint", Have: exp == "true",
		Detail:  "daemon experimental=" + orUnknown(exp),
		Meaning: "sbx checkpoint / resume is unavailable; sleeping and forking keep the disk, not the process",
	})

	kubectlOK, kubectlWhere := have("kubectl")
	rep.Capabilities = append(rep.Capabilities, Capability{
		Name: "kubectl", Have: kubectlOK, Detail: kubectlWhere,
		Meaning: "--provider kubernetes is unavailable",
	})

	for _, t := range []struct{ name, why string }{
		{"cloudflared", "sbx url falls back to another tunnel backend"},
		{"redis-cli", "only affects selftest and the benchmarks"},
	} {
		ok, where := have(t.name)
		rep.Capabilities = append(rep.Capabilities, Capability{
			Name: t.name, Have: ok, Detail: where, Meaning: t.why,
		})
	}

	return rep
}

// PrintReport writes the report for a human, or as JSON for anything else. The exit status
// is the caller's business: a missing cloudflared is not a broken machine, and deciding
// which absences matter belongs to whoever is reading.
func PrintReport(w io.Writer, rep Report, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")

		return enc.Encode(rep)
	}

	fmt.Fprintf(w, "host %s\n\n", rep.Host)

	for _, c := range rep.Capabilities {
		mark := "✗"
		if c.Have {
			mark = "✓"
		}

		fmt.Fprintf(w, "  %s %-18s %s\n", mark, c.Name, c.Detail)

		if !c.Have && c.Meaning != "" {
			fmt.Fprintf(w, "    %-18s %s\n", "", c.Meaning)
		}
	}

	return nil
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}

	return s
}

// machineRow is the "this machine" entry. Its Meaning is what the absence costs, so it is set
// only when memory could not be read - a row that printed the memory and also said sbx cannot
// read it contradicted itself. Where sbx does read memory (macOS and Linux), a missing figure is
// a read that failed, and says where to look; elsewhere it is unsupported, and says that.
func machineRow(goos string, mach hostinfo.Machine) Capability {
	detail := fmt.Sprintf("%d cores", mach.Cores)
	if mach.Cores == 1 {
		detail = "1 core"
	}

	c := Capability{Name: "this machine", Have: mach.MemBytes > 0}

	if c.Have {
		detail += fmt.Sprintf(", %.0f GB of memory", float64(mach.MemBytes)/(1<<30))

		if mach.FreeBytes > 0 {
			detail += fmt.Sprintf(", %.1f GB free now", float64(mach.FreeBytes)/(1<<30))
		}

		c.Detail = detail

		return c
	}

	c.Detail = detail + ", memory unknown"

	const costs = "how much room there is for sandboxes: `sbx ui` shows the container runtime's figures alone"

	switch goos {
	case "linux":
		c.Meaning = costs + ". /proc/meminfo could not be read or had no MemTotal"
	case "darwin":
		c.Meaning = costs + ". `sysctl hw.memsize` gave no answer"
	default:
		c.Meaning = costs + ". sbx cannot read memory on " + goos
	}

	return c
}

// daemonCapability is doctor's "sbx serve" row.
func daemonCapability() Capability {
	if p, running := daemon.Running(); running {
		return Capability{
			Name: "sbx serve", Have: true,
			Detail:  fmt.Sprintf("pid %d, since %s, provider %s", p.PID, p.Since.Format("15:04"), p.Provider),
			Meaning: "the ports `sbx env` exports are being fronted",
		}
	}

	// Only daemons started with --only: they front their scope and nothing else, so a sandbox
	// outside it still has no daemon - which the row says rather than claiming either extreme.
	if scoped := daemon.Scoped(); len(scoped) > 0 {
		var parts []string

		for _, p := range scoped {
			parts = append(parts, fmt.Sprintf("pid %d --only %s", p.PID, p.Scope))
		}

		return Capability{
			Name: "sbx serve", Have: true,
			Detail:  "scoped only: " + strings.Join(parts, "; "),
			Meaning: "only sandboxes inside those scopes are fronted; for any other, start one: sbx serve --idle 5m &",
		}
	}

	return Capability{
		Name: "sbx serve", Have: false, Detail: "not running",
		Meaning: "nothing accepts on the ports `sbx env` exports; start one: sbx serve --idle 5m &",
	}
}
