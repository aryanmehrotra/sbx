package cli

import (
	"fmt"
	"io"

	"github.com/aryanmehrotra/sbx/internal/slotlock"
)

// gcLocks lists the slot and sandbox-name locks whose holder is gone, and removes them with
// --force. A create that was killed leaves its name's lock behind; the next create of that name
// clears it, but a one-off name - a CI job's - is never used again, so nothing else would.
// Harmless while it sits there, which is why it follows gc's list-then-force rule like the rest
// rather than being removed on sight. A live holder's lock is never offered.
func gcLocks(w io.Writer, force bool) error {
	stale, err := slotlock.Stale()
	if err != nil || len(stale) == 0 {
		return nil // a machine with no usable $HOME has no lock files to be stale
	}

	fmt.Fprintln(w)

	for _, p := range stale {
		fmt.Fprintf(w, "  %-58s %s\n", p, "stale lock (holder not running)")
	}

	if !force {
		fmt.Fprintf(w, "\n%d stale lock(s), nothing removed. Add --force to remove them.\n", len(stale))

		return nil
	}

	if err := slotlock.RemoveStale(stale); err != nil {
		return fmt.Errorf("could not remove stale locks: %w", err)
	}

	fmt.Fprintf(w, "\nremoved %d stale lock(s)\n", len(stale))

	return nil
}
