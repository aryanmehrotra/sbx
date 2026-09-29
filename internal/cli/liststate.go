package cli

import "github.com/aryanmehrotra/sbx/internal/provider"

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
