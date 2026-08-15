package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/aryanmehrotra/sbx/internal/spec"
)

// Checking a spec without creating anything.
//
// sandbox.json is a committed file, so a change to it can break every branch at once — and
// until now the only way to find out was `sbx create`, which needs a docker daemon, pulls
// images and leaves containers behind. That is not something a pre-commit hook or a lint job
// can run, so in practice nobody checked.
//
// This runs the same loader `create` does, which is the only way it is worth having: a
// separate validator would drift, and a spec that passes lint and fails create is worse than
// no lint at all.

// Validate loads a spec and reports what it declares, without touching a provider.
func Validate(w io.Writer, path string) error {
	sp, err := spec.LoadSpec(path)
	if err != nil {
		return err
	}

	layout, err := sp.Assign()
	if err != nil {
		return err
	}

	order, err := sp.CreationOrder()
	if err != nil {
		return err
	}

	fmt.Fprintf(w, "%s — %d service(s), spec version %d\n\n", path, len(sp.Services), sp.Version)

	for _, name := range order {
		svc := sp.Services[name]

		what := svc.Image
		if svc.Build != nil {
			what = "build " + svc.Build.Context
		}

		ports := make([]string, 0, len(svc.Ports))
		for _, p := range svc.Ports {
			ports = append(ports, fmt.Sprint(p))
		}

		start, _ := sp.StartIndex(layout, name)

		fmt.Fprintf(w, "  %-14s %-48s ports %-12s ordinal %d\n",
			name, what, strings.Join(ports, ","), start)

		if len(svc.DependsOn) > 0 {
			fmt.Fprintf(w, "  %-14s   after %s\n", "", strings.Join(svc.DependsOn, ", "))
		}

		// Silence about a missing health check would be the wrong kind of quiet: it is the
		// difference between a wake that is verified and one that guesses, and it is the
		// single most common reason a sandbox behaves oddly.
		if svc.Health == "" {
			fmt.Fprintf(w, "  %-14s   no health command — the daemon can only dial the port, "+
				"and docker answers that before the server does\n", "")
		}
	}

	if len(sp.Exports) > 0 {
		fmt.Fprintln(w)

		for _, env := range sortedKeys(sp.Exports) {
			fmt.Fprintf(w, "  exports %-20s → %s\n", env, sp.Exports[env])
		}
	}

	fmt.Fprintf(w, "\nvalid. Nothing was created — this only reads the file.\n")

	return nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}
