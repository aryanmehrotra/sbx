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

	// Both columns fit the widest thing in them, so one long name or stability does not push
	// every other row out of line.
	wide, state := len("FEATURE"), len("off (experimental)")
	for _, f := range all {
		if len(f.Name) > wide {
			wide = len(f.Name)
		}
	}

	fmt.Fprintf(w, "%-*s %-*s %s\n", wide, "FEATURE", state, "STATE", "WHAT IT IS")

	for _, f := range all {
		on := "off"
		if features.Enabled(f.Name) {
			on = "on"
		}

		fmt.Fprintf(w, "%-*s %-*s %s\n", wide, f.Name, state, on+" ("+string(f.Stability)+")", f.Summary)

		if f.Caveat != "" {
			fmt.Fprintf(w, "%-*s unproven: %s\n", wide+state+1, "", f.Caveat)
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

func init() {
	features.Register(features.Feature{
		Name:      "devcontainer",
		Stability: features.Preview,
		Summary:   "`sbx init --from-devcontainer` - a spec from the .devcontainer a repo already has",
	})
}

func init() {
	features.Register(features.Feature{
		Name:      "waiting-page",
		Stability: features.Preview,
		Summary:   "a browser opening a sleeping `sbx url` gets a page saying so, instead of a spinner",
	})
}
