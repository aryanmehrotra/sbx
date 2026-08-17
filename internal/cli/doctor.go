package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"runtime"

	"github.com/aryanmehrotra/sbx/internal/hostinfo"
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
	mach := hostinfo.Read()

	detail := fmt.Sprintf("%d cores", mach.Cores)
	if mach.Cores == 1 {
		detail = "1 core"
	}

	if mach.MemBytes > 0 {
		detail += fmt.Sprintf(", %.0f GB of memory", float64(mach.MemBytes)/(1<<30))

		if mach.FreeBytes > 0 {
			detail += fmt.Sprintf(", %.1f GB free now", float64(mach.FreeBytes)/(1<<30))
		}
	} else {
		detail += ", memory unknown"
	}

	rep.Capabilities = append(rep.Capabilities, Capability{
		Name: "this machine", Have: mach.MemBytes > 0, Detail: detail,
		Meaning: "how much room there is for sandboxes. sbx cannot read memory on " +
			runtime.GOOS + ", so `sbx ui` shows the container runtime's figures alone",
	})

	dockerOK, dockerWhere := have("docker")
	rep.Capabilities = append(rep.Capabilities, Capability{
		Name: "docker", Have: dockerOK, Detail: dockerWhere,
		Meaning: "the default provider; without it use --provider kubernetes",
	})

	if dockerOK {
		ver, up := dockerInfo(ctx, "{{.ServerVersion}}")
		detail := ver

		if !up {
			detail = "the CLI is here but the daemon did not answer"
		}

		rep.Capabilities = append(rep.Capabilities, Capability{
			Name: "docker daemon", Have: up, Detail: detail,
			Meaning: "nothing can be created until the daemon is running",
		})
	}

	// The one capability whose absence breaks everything, and it was the one doctor did not
	// report. README says the daemon "owns the ports sbx env hands out, so nothing works
	// without it", and then doctor listed a missing redis-cli that only affects selftest
	// while saying nothing about this. A daemon started with `sbx serve &` dies with the
	// terminal, and the first thing anyone runs afterwards is doctor.
	if p, running := daemon.Running(); running {
		rep.Capabilities = append(rep.Capabilities, Capability{
			Name: "sbx serve", Have: true,
			Detail:  fmt.Sprintf("pid %d, since %s, provider %s", p.PID, p.Since.Format("15:04"), p.Provider),
			Meaning: "the ports `sbx env` exports are being fronted",
		})
	} else {
		rep.Capabilities = append(rep.Capabilities, Capability{
			Name: "sbx serve", Have: false, Detail: "not running",
			Meaning: "nothing accepts on the ports `sbx env` exports; start one: sbx serve --idle 5m &",
		})
	}

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

	// Checkpoint/restore, which is what a memory-preserving sleep would need. Two things
	// have to be true and they fail differently, so both are reported.
	exp, _ := dockerInfo(ctx, "{{.ExperimentalBuild}}")
	rep.Capabilities = append(rep.Capabilities, Capability{
		Name: "docker checkpoint", Have: exp == "true",
		Detail:  "daemon experimental=" + orUnknown(exp),
		Meaning: "memory-state sleep is unavailable; sleeping keeps the disk, not the process",
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
