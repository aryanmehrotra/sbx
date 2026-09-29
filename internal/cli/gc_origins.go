package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// Origin records whose sandbox is gone.
//
// `sbx rm` forgets a sandbox's record (origin.go) - but only a sandbox it finds. A create that
// failed and removed the sandbox's only container left ~/.sbx/origins/<name>.json behind, and
// `sbx rm <name>` then answered "no sandbox" and left it there for good. Harmless while it sits
// there (Recall is only ever a default), which is why gc follows its list-then-force rule for it
// like it does for stale locks.
//
// A snapshot has a record too - Inherit gives it its source's, so a fork of it knows its spec -
// so a name with snapshot images behind it is never an orphan, and a name this cannot check is
// kept.

// RemoveMissing is `sbx rm` of a name with no sandbox: it clears that name's leftover origin
// record and says so, or, with nothing to clear, is the unknown-sandbox error it always was.
func RemoveMissing(ctx context.Context, p provider.Provider, sandbox string) error {
	path, err := originPath(sandbox)
	if err != nil || !fileExists(path) || hasSnapshot(ctx, p, sandbox) {
		return UnknownSandbox(ctx, p, sandbox)
	}

	if err := os.Remove(path); err != nil {
		return fmt.Errorf("no sandbox %q, and its leftover origin record %s could not be removed: %w", sandbox, path, err)
	}

	fmt.Printf("no sandbox %q; removed its leftover origin record %s\n", sandbox, path)

	return nil
}

// gcOrigins lists the origin records with no sandbox and no snapshot of their name, and removes
// them with --force.
func gcOrigins(ctx context.Context, p provider.Provider, w io.Writer, force bool) error {
	path, err := originPath("x")
	if err != nil {
		return nil // no usable $HOME: no records to be orphans
	}

	files, err := filepath.Glob(filepath.Join(filepath.Dir(path), "*.json"))
	if err != nil || len(files) == 0 {
		return nil
	}

	units, err := p.List(ctx, "")
	if err != nil {
		return nil // cannot tell which sandboxes exist, so none is offered
	}

	live := map[string]bool{}
	for _, u := range units {
		live[u.Sandbox] = true
	}

	var orphans []string

	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".json")
		if live[name] || hasSnapshot(ctx, p, name) {
			continue
		}

		orphans = append(orphans, f)
	}

	if len(orphans) == 0 {
		return nil
	}

	sort.Strings(orphans)

	fmt.Fprintln(w)

	for _, f := range orphans {
		fmt.Fprintf(w, "  %-58s %s\n", f, "origin record (no sandbox or snapshot of that name)")
	}

	if !force {
		fmt.Fprintf(w, "\n%d orphan origin record(s), nothing removed. Add --force to remove them.\n", len(orphans))

		return nil
	}

	for _, f := range orphans {
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("could not remove origin record %s: %w", f, err)
		}
	}

	fmt.Fprintf(w, "\nremoved %d orphan origin record(s)\n", len(orphans))

	return nil
}

// hasSnapshot reports whether snapshot images of this name exist, or whether that cannot be
// told - either way its record is kept. A provider that cannot snapshot has none.
func hasSnapshot(ctx context.Context, p provider.Provider, name string) bool {
	snap, ok := p.(provider.Snapshotter)
	if !ok {
		return false
	}

	images, err := snap.Images(ctx, "sbx-snap-"+name+"-")

	return err != nil || len(images) > 0
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
