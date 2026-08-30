package app

// The gates this build has, and the command that shows them.

import (
	"fmt"
	"io"
	"strings"

	"github.com/aryanmehrotra/sbx/internal/features"
)

func init() {
	features.Register(features.Feature{
		Name:      "ssh",
		Stability: features.Preview,
		Summary:   "`sbx ssh` - reach a sandbox with an editor, over the wake path that already exists",
	})
}

// showFeatures prints what exists, what is on, and what was asked for that is not real.
func showFeatures(w io.Writer) {
	all := features.All()

	if len(all) == 0 {
		fmt.Fprintln(w, "no gated features in this build - everything it does is on.")

		return
	}

	fmt.Fprintf(w, "%-14s %-14s %s\n", "FEATURE", "STATE", "WHAT IT IS")

	for _, f := range all {
		state := "off"
		if features.Enabled(f.Name) {
			state = "on"
		}

		fmt.Fprintf(w, "%-14s %-14s %s\n", f.Name, state+" ("+string(f.Stability)+")", f.Summary)

		if f.Caveat != "" {
			fmt.Fprintf(w, "%-29s unproven: %s\n", "", f.Caveat)
		}
	}

	fmt.Fprintf(w, "\nTurn one on for a command:  SBX_FEATURES=%s sbx ...\n", all[0].Name)
	fmt.Fprintln(w, "There is no switch for all of them - each is a separate decision.")

	// A name that is not a feature does nothing, and doing nothing quietly reads as the
	// feature being broken rather than the name being wrong.
	if unknown := features.Unknown(); len(unknown) > 0 {
		fmt.Fprintf(w, "\nSBX_FEATURES names %d thing(s) this build does not have: %s\n",
			len(unknown), strings.Join(unknown, ", "))
	}
}
