package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/aryanmehrotra/sbx/internal/daemon"
	"github.com/aryanmehrotra/sbx/internal/egress"
	"github.com/aryanmehrotra/sbx/internal/provider"
)

// EgressChange is what `sbx egress` was asked to do, applied in the order a person would read it:
// back to the spec, then removals, then the default, then new rules.
type EgressChange struct {
	Reset   bool
	Remove  []string
	Default string

	// Rules keep the order they were typed in, across --allow and --deny, because matching is
	// first-match: `--deny a.example.com --allow '*.example.com'` means something different
	// from the same two flags the other way round.
	Rules []egress.Rule

	JSON bool
}

// Egress reads or changes the egress policy of a running sandbox. With no change it shows it.
func Egress(ctx context.Context, p provider.Provider, sandbox, service string, ch EgressChange) error {
	return egressTo(ctx, os.Stdout, daemon.NewEgressControl(p, ""), sandbox, service, ch)
}

func egressTo(ctx context.Context, w io.Writer, c *daemon.EgressControl, sandbox, service string, ch EgressChange) error {
	st, err := c.GetPolicy(ctx, sandbox, service)
	if err != nil {
		return err
	}

	if ch.Reset {
		if st, err = c.ResetPolicy(ctx, sandbox, service); err != nil {
			return err
		}
	}

	if len(ch.Remove) > 0 {
		if st, err = c.DeleteRules(ctx, sandbox, service, ch.Remove); err != nil {
			return err
		}
	}

	if ch.Default != "" {
		if st, err = c.SetDefault(ctx, sandbox, service, ch.Default); err != nil {
			return err
		}
	}

	if len(ch.Rules) > 0 {
		if st, err = c.PatchPolicy(ctx, sandbox, service, ch.Rules); err != nil {
			return err
		}
	}

	if ch.JSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")

		return enc.Encode(st)
	}

	printEgress(w, sandbox, st)

	return nil
}

func printEgress(w io.Writer, sandbox string, st egress.Status) {
	p := st.Policy
	if p == nil {
		return
	}

	fmt.Fprintf(w, "egress for %s: %s, default %s\n", sandbox, st.Mode, p.DefaultAction)

	if len(p.Egress) > 0 {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)

		for i, r := range p.Egress {
			fmt.Fprintf(tw, "  %d\t%s\t%s\n", i+1, r.Action, r.Target)
		}

		_ = tw.Flush()

		fmt.Fprintln(w, "  first matching name rule wins; an address rule that denies wins over one that allows")
	}

	if st.Reason != "" {
		fmt.Fprintf(w, "  %s\n", st.Reason)
	}
}
