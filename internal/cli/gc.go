package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// Reclaiming what sandboxes leave behind.
//
// sbx sleeps a sandbox to 0 B and has never had any concept of expiring one. A volume
// outlives its sandbox by design — that is what makes sleeping safe — but nothing ever
// removed it once the sandbox was gone, so a machine that has run a sandbox per branch for
// a month is carrying every branch it ever had. That is the limit that binds first on a
// laptop: the disk fills long before a wake latency matters.
//
// Two rules, because deleting data is the one operation that cannot be taken back:
//
//   - It lists by default and deletes only when told. A garbage collector that runs by
//     surprise is one nobody can safely put in a cron.
//   - It only ever offers something whose sandbox no longer exists. A sleeping sandbox is
//     not garbage — being asleep is the normal state here, and reclaiming one would delete
//     the data of a branch somebody comes back to on Monday.
//
// Snapshots are listed separately and never swept by default. They were made on purpose,
// by name, and the whole point of one is that it outlives the sandbox it came from.

// GC finds reclaimable artifacts and, if asked, reclaims them.
func GC(ctx context.Context, p provider.Provider, w io.Writer, olderThan time.Duration, force, withSnapshots bool) error {
	col, err := provider.CollectorFor(p)
	if err != nil {
		return err
	}

	return gcWith(ctx, col, w, olderThan, force, withSnapshots)
}

// gcWith is the part worth testing, separated from finding the collector so the rules can
// be exercised without a docker daemon — the rules are about what is OFFERED and what is
// deleted, and neither needs a real volume to get wrong.
func gcWith(ctx context.Context, col provider.Collector, w io.Writer, olderThan time.Duration, force, withSnapshots bool) error {
	items, err := col.Orphans(ctx)
	if err != nil {
		return err
	}

	var (
		sweep []provider.Artifact
		kept  int
	)

	for _, a := range items {
		if a.Snapshot && !withSnapshots {
			kept++
			continue
		}

		if a.Age < olderThan {
			kept++
			continue
		}

		sweep = append(sweep, a)
	}

	if len(sweep) == 0 {
		fmt.Fprintf(w, "nothing to reclaim")

		if kept > 0 {
			fmt.Fprintf(w, " (%d skipped: newer than %s, or a snapshot)", kept, olderThan)
		}

		fmt.Fprintln(w)

		return nil
	}

	for _, a := range sweep {
		what := a.Kind
		if a.Snapshot {
			what += ", snapshot"
		}

		fmt.Fprintf(w, "  %-40s %-18s %s\n", a.Name, what, age(a.Age))
	}

	if !force {
		fmt.Fprintf(w, "\n%d reclaimable, nothing deleted. Add --force to delete them.\n", len(sweep))

		if kept > 0 {
			fmt.Fprintf(w, "%d more were skipped for being newer than %s, or for being snapshots "+
				"(--snapshots includes those).\n", kept, olderThan)
		}

		return nil
	}

	var failed []string

	for _, a := range sweep {
		if err := col.Reclaim(ctx, a); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", a.Name, err))
		}
	}

	fmt.Fprintf(w, "\nreclaimed %d of %d\n", len(sweep)-len(failed), len(sweep))

	if len(failed) > 0 {
		return fmt.Errorf("could not reclaim: %s", strings.Join(failed, "; "))
	}

	return nil
}

// age prints a duration the way someone deciding whether to delete something reads it.
func age(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm old", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh old", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd old", int(d.Hours()/24))
	}
}
