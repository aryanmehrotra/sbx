package cli

// Turning a repository's existing .devcontainer into a spec.
//
// Adoption, not feature parity. A repo with `.devcontainer/` has already answered most of what
// sandbox.json asks - what to run, what to publish, what to mount, what to do once it is up -
// and making somebody write it again in a different shape is the reason they do not try the tool.
//
// What it cannot carry, it prints. A partial import that stays quiet about what it dropped is how
// somebody spends an afternoon on a missing binary that a Feature was supposed to install.

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/aryanmehrotra/sbx/internal/devcontainer"
	"github.com/aryanmehrotra/sbx/internal/spec"
)

// InitFromDevcontainer writes a spec to w, and anything it could not translate to notes.
//
// The spec goes to stdout and the notes to stderr, so `sbx init --from-devcontainer . >
// sandbox.json` produces a clean file and still tells the reader what was left behind. That
// split is the whole reason the notes are not comments in the JSON.
func InitFromDevcontainer(path string, w, notes io.Writer) error {
	got, err := devcontainer.Load(path)
	if err != nil {
		return err
	}

	sp := spec.Spec{
		Version:  1,
		Services: map[string]spec.Service{got.Service: got.Spec},
	}

	out, err := json.MarshalIndent(sp, "", "  ")
	if err != nil {
		return err
	}

	if _, err := fmt.Fprintf(w, "%s\n", out); err != nil {
		return err
	}

	fmt.Fprintf(notes, "\ntranslated a devcontainer into one service, %q.\n", got.Service)

	if len(got.Dropped) > 0 {
		fmt.Fprintf(notes, "\n%d thing(s) did not come across:\n", len(got.Dropped))

		for _, d := range got.Dropped {
			fmt.Fprintf(notes, "  - %s\n", d)
		}
	}

	fmt.Fprintf(notes, "\nA devcontainer describes one container; sandbox.json describes the\n"+
		"services a branch needs. Add the database and the cache beside it.\n")

	return nil
}
