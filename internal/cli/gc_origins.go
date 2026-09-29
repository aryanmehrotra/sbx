package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
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
//
// Only a record this backend owns (originKey). A record naming another backend is that backend's
// sandbox, which this one simply cannot see - kubernetes, firecracker, another docker engine - and
// is left alone. A record naming none was written before records said, and cannot be attributed,
// so it is never removed for you: the error says where it is and how to remove it by hand.
func RemoveMissing(ctx context.Context, p provider.Provider, sandbox string) error {
	path, err := originPath(sandbox)
	if err != nil || !fileExists(path) || hasSnapshot(ctx, p, sandbox) {
		return UnknownSandbox(ctx, p, sandbox)
	}

	switch owner := readOrigin(path).Provider; owner {
	case originKey(p):
	case "":
		return fmt.Errorf("no sandbox %q on %s. Its origin record %s names no provider - it was written "+
			"before sbx recorded one - so sbx cannot tell whether another backend still has that sandbox, "+
			"and leaves it. If none does, remove it by hand:  rm %s", sandbox, p.Name(), path, path)
	default:
		return UnknownSandbox(ctx, p, sandbox) // another backend's sandbox: not missing, just not here
	}

	if err := os.Remove(path); err != nil {
		return fmt.Errorf("no sandbox %q, and its leftover origin record %s could not be removed: %w", sandbox, path, err)
	}

	fmt.Printf("no sandbox %q; removed its leftover origin record %s\n", sandbox, path)

	return nil
}

// originListMax is how many orphan records gc names before it only counts the rest. A machine
// that has run CI names for a year has hundreds, and a screen of paths buries the volumes above it.
const originListMax = 10

// gcOrigins lists the origin records this backend owns with no sandbox and no snapshot of their
// name, and removes them with --force. Another backend's records are not its to judge and are not
// mentioned; records naming no provider are counted, with the manual command, and never removed.
func gcOrigins(ctx context.Context, p provider.Provider, w io.Writer, force bool) error {
	path, err := originPath("x")
	if err != nil {
		return nil // no usable $HOME: no records to be orphans
	}

	dir := filepath.Dir(path)

	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
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

	snapped, snapErr := snapshotImages(ctx, p)
	if snapErr != nil {
		return nil // cannot tell which names are snapshots, so none is offered
	}

	me := originKey(p)

	var orphans, legacy []string

	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".json")
		if live[name] || slices.ContainsFunc(snapped, func(img string) bool {
			return strings.HasPrefix(img, "sbx-snap-"+name+"-")
		}) {
			continue
		}

		switch readOrigin(f).Provider {
		case me:
			orphans = append(orphans, f)
		case "":
			legacy = append(legacy, f)
		}
	}

	sort.Strings(orphans)

	if len(orphans) > 0 {
		fmt.Fprintf(w, "\n  %d origin record(s) in %s with no sandbox or snapshot of that name on %s:\n",
			len(orphans), dir, me)

		for i, f := range orphans {
			if i == originListMax {
				fmt.Fprintf(w, "    ... and %d more\n", len(orphans)-originListMax)
				break
			}

			fmt.Fprintf(w, "    %s\n", strings.TrimSuffix(filepath.Base(f), ".json"))
		}
	}

	if len(legacy) > 0 {
		fmt.Fprintf(w, "\n  %d origin record(s) in %s name no provider (written before sbx recorded one), so gc "+
			"cannot tell whose they are and never removes them. Any whose sandbox no backend has can go by "+
			"hand:  rm %s/<name>.json\n", len(legacy), dir, dir)
	}

	if len(orphans) == 0 {
		return nil
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

// snapshotImages is every snapshot image, once, for gc to match each record against: one Images
// call rather than one per record, which on this machine was 134.
func snapshotImages(ctx context.Context, p provider.Provider) ([]string, error) {
	snap, ok := p.(provider.Snapshotter)
	if !ok {
		return nil, nil
	}

	return snap.Images(ctx, "sbx-snap-")
}

// readOrigin reads a record as written, provider and all. Unreadable reads as naming no provider,
// which is never removed.
func readOrigin(path string) Origin {
	var o Origin

	if body, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(body, &o)
	}

	return o
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
