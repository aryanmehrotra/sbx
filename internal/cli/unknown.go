package cli

// What to say when somebody names a sandbox that is not there.
//
// `no sandbox "feture-x"` is correct and unhelpful. The mistake is nearly always a typo or a
// sandbox somebody else removed, and in both cases the useful next thing is the list of names
// that do exist - which the caller already has in hand, because it just asked for it.
//
// This matters more than it looks. The alternative is that the reader runs `sbx list` to find
// out, and the whole point of an error message is to save them that round trip.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// UnknownSandbox builds the error for a name that does not exist, naming the ones that do.
//
// Exported because the provider layer produces a bare "no sandbox" for the same situation and
// should not: a backend knows whether a container exists, not what a person should read.
func UnknownSandbox(ctx context.Context, p provider.Provider, sandbox string) error {
	others := otherSandboxes(ctx, p, sandbox)

	switch {
	case len(others) == 0:
		return fmt.Errorf("no sandbox %q, and there are none on this machine yet.\n"+
			"     create it:  sbx create %s --template postgres     (sbx templates lists them)",
			sandbox, sandbox)

	case len(others) <= 8:
		return fmt.Errorf("no sandbox %q. These exist: %s\n"+
			"     create it:  sbx create %s --template postgres",
			sandbox, strings.Join(others, ", "), sandbox)

	default:
		// Past a handful the list stops being a hint and starts being output to scroll
		// through, which is what `sbx list` is for.
		return fmt.Errorf("no sandbox %q. There are %d others - `sbx list` names them",
			sandbox, len(others))
	}
}

// otherSandboxes is every sandbox except the one asked for, deduplicated: List returns one
// unit per service, and a web-stack would otherwise offer the same name three times.
func otherSandboxes(ctx context.Context, p provider.Provider, except string) []string {
	units, err := p.List(ctx, "")
	if err != nil {
		return nil // the error being reported is the missing sandbox, not this
	}

	seen := map[string]bool{}

	var out []string

	for _, u := range units {
		if u.Sandbox == except || seen[u.Sandbox] {
			continue
		}

		seen[u.Sandbox] = true

		out = append(out, u.Sandbox)
	}

	sort.Strings(out)

	return out
}
