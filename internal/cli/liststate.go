package cli

import (
	"fmt"
	"io"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// unitState is the STATE column of `sbx list` and the "state" field of `sbx list --json`.
//
// Three values, not two, because asleep and frozen cost different things. A stopped service holds
// no memory; a frozen one (on_idle: freeze, the container paused) keeps its processes and memory
// and resumes without a restart. Both answered "asleep" here, so a freeze user could not tell
// from the listing whether a sandbox was still holding RAM.
//
// "awake" in JSON keeps its old meaning - running, able to answer - so a frozen service is
// awake:false there, as it always was, and callers filtering on it see no change.
func unitState(u provider.Unit) string {
	switch {
	case u.Running:
		return "awake"
	case u.Paused:
		return "frozen"
	default:
		return "asleep"
	}
}

// listTable is the human `sbx list`, already sorted by the caller.
func listTable(w io.Writer, units []provider.Unit, backend string) {
	fmt.Fprintf(w, "%-20s %-14s %-9s %-11s %s\n", "SANDBOX", "SERVICE", "STATE", "ISOLATION", "ADDRESS")

	for _, u := range units {
		iso := unitIsolation(u, backend)
		if iso == "" {
			iso = "-"
		}

		fmt.Fprintf(w, "%-20s %-14s %-9s %-11s %s\n", u.Sandbox, u.Service, unitState(u), iso, joinEndpoints(u.Client))
	}
}

// unitIsolation is the ISOLATION column of `sbx list` and the "isolation" field of `--json`: the
// tier on the unit's sbx.isolation label. A docker unit without one predates the label and was
// created as a container; a firecracker unit is a microVM whatever it records. Any other provider
// records nothing to read, and "" says so rather than guessing.
func unitIsolation(u provider.Unit, backend string) string {
	switch {
	case u.Isolation != "":
		return string(u.Isolation)
	case backend == "docker":
		return string(provider.IsolationContainer)
	case backend == "firecracker":
		return string(provider.IsolationFirecracker)
	default:
		return ""
	}
}
