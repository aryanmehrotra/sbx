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
// outlives its sandbox by design - that is what makes sleeping safe - but nothing ever
// removed it once the sandbox was gone, so a machine that has run a sandbox per branch for
// a month is carrying every branch it ever had. That is the limit that binds first on a
// laptop: the disk fills long before a wake latency matters.
//
// Two rules, because deleting data is the one operation that cannot be taken back:
//
//   - It lists by default and deletes only when told. A garbage collector that runs by
//     surprise is one nobody can safely put in a cron.
//   - It only ever offers something whose sandbox no longer exists. A sleeping sandbox is
//     not garbage - being asleep is the normal state here, and reclaiming one would delete
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
// be exercised without a docker daemon - the rules are about what is OFFERED and what is
// deleted, and neither needs a real volume to get wrong.
func gcWith(ctx context.Context, col provider.Collector, w io.Writer, olderThan time.Duration, force, withSnapshots bool) error {
	items, err := col.Orphans(ctx)
	if err != nil {
		return err
	}

	var (
		sweep     []provider.Artifact
		snapshots kinds // skipped because snapshots are opt-in
		inUse     kinds // skipped because a sandbox still runs from or mounts it
		tooNew    int   // skipped because of --older-than
	)

	for _, a := range items {
		if a.Snapshot && !withSnapshots {
			snapshots.add(a.Kind)
			continue
		}

		// Before age, and regardless of --force: a fork is created from its snapshot's images,
		// so they outlive the sandbox the snapshot was taken from. Listing them as reclaimable
		// was wrong, and --force then deleted what docker let go and failed on the rest,
		// leaving a snapshot with some of its services for the next fork to start from.
		if a.InUse {
			inUse.add(a.Kind)
			continue
		}

		if a.Age < olderThan {
			tooNew++
			continue
		}

		sweep = append(sweep, a)
	}

	if len(sweep) == 0 {
		fmt.Fprint(w, "nothing to reclaim")

		if why := skipped(snapshots, inUse, tooNew, olderThan); why != "" {
			fmt.Fprintf(w, " (%s)", why)
		}

		fmt.Fprintln(w)

		return nil
	}

	var noImage []provider.Artifact

	for _, a := range sweep {
		what := a.Kind
		if a.Snapshot {
			what += ", snapshot"
		}

		if a.NoImage {
			what += ", no image"
			noImage = append(noImage, a)
		}

		fmt.Fprintf(w, "  %-40s %-26s %s\n", a.Name, what, age(a.Age))
	}

	// Named, because a volume with no image reads as half of a snapshot that is still there, and
	// it is not: `sbx snapshot` was killed mid-copy, before any image. The snapshot is the one its
	// label names. Never read from the volume name: sbx-snapvol-a-web-ui is "a"'s web-ui or
	// "a-web"'s ui, and a guessed `--rm` removes the wrong snapshot or none. Unlabelled (made
	// before snapshot volumes carried one), the command that is always right is removing it.
	for _, a := range noImage {
		fix := "docker volume rm " + a.Name
		if a.SnapshotName != "" {
			fix = "sbx snapshot --rm " + a.SnapshotName
		}

		fmt.Fprintf(w, "  %s has no image: an interrupted snapshot left it. %s removes it.\n", a.Name, fix)
	}

	if !force {
		fmt.Fprintf(w, "\n%d reclaimable, nothing deleted. Add --force to delete them.\n", len(sweep))

		if why := skipped(snapshots, inUse, tooNew, olderThan); why != "" {
			fmt.Fprintf(w, "Also %s.\n", why)
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

	// Said on the deleting run too: the reader of a --force in a cron log is the one who most
	// needs to know a snapshot was kept, and why.
	if why := skipped(snapshots, inUse, tooNew, olderThan); why != "" {
		fmt.Fprintf(w, "Also %s.\n", why)
	}

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

// skipped says why artifacts were left out, one count per reason, and nothing for a reason that
// did not apply. It used to be one lumped number - "24 more were skipped for being newer than 0s,
// or for being snapshots" - where, with no --older-than, the first reason was impossible and the
// reader could not tell how many of the 24 a --snapshots run would add.
func skipped(snapshots, inUse kinds, tooNew int, olderThan time.Duration) string {
	var parts []string

	if !snapshots.none() {
		parts = append(parts, snapshots.String()+" of snapshots skipped (--snapshots includes them)")
	}

	if !inUse.none() {
		parts = append(parts, inUse.String()+" in use by a sandbox skipped (sbx rm the sandbox that uses it first)")
	}

	if tooNew > 0 {
		parts = append(parts, fmt.Sprintf("%d newer than %s skipped (--older-than)", tooNew, olderThan))
	}

	return strings.Join(parts, ", ")
}

// kinds counts skipped artifacts by kind. The summary used to say "29 snapshots skipped" for
// what were images and volumes together, so one pg+redis snapshot read as four snapshots.
// Counting whole snapshots would mean resolving each artifact to its snapshot's name, which an
// unlabelled image or a volume cannot always give; saying what was counted cannot be wrong.
type kinds struct{ images, volumes int }

func (k *kinds) add(kind string) {
	if kind == "image" {
		k.images++
	} else {
		k.volumes++
	}
}

func (k kinds) none() bool { return k.images == 0 && k.volumes == 0 }

func (k kinds) String() string {
	count := func(n int, noun string) string {
		if n == 1 {
			return "1 " + noun
		}

		return fmt.Sprintf("%d %ss", n, noun)
	}

	switch {
	case k.volumes == 0:
		return count(k.images, "image")
	case k.images == 0:
		return count(k.volumes, "volume")
	default:
		return count(k.images, "image") + " and " + count(k.volumes, "volume")
	}
}
