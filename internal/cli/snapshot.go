package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// Snapshot and fork.
//
// The interesting capability the hosted platforms have is not "resume the one you paused" -
// it is spawning many sandboxes from one saved state. That is what makes a sandbox per task
// affordable: seed a database once, migrate it once, then hand every agent its own copy.
//
// This is a FILESYSTEM snapshot. Processes and memory are not in it: a fork starts its
// services cold against a warm disk, exactly as a wake does. E2B and zeropod restore memory
// too - about a second for E2B, and 272 ms for zeropod when this project measured it -
// using Firecracker or CRIU, neither of
// which exists on a machine that only has docker, and `sbx doctor` will tell you whether
// yours does.
//
// It snapshots the VOLUME, not the container's filesystem, and that distinction was found
// the hard way. `docker commit` looks like the obvious primitive and does not capture
// mounted volumes at all: committing a seeded postgres produced an image whose data
// directory held zero files against the live container's twenty-four. Every byte worth
// snapshotting in sbx is in a volume, because `volume` is the field that makes sleeping
// safe. The image is still committed for services that keep state outside one.
//
// Saying "snapshot" while meaning only the disk is how a benchmark table ends up comparing
// two different things, so every name here says filesystem and the docs say it twice.

// SnapshotRef is one service's saved image.
type SnapshotRef struct {
	Service string `json:"service"`
	Image   string `json:"image"`
	Volume  string `json:"volume,omitempty"`
}

func snapshotVolume(name, service string) string {
	return "sbx-snapvol-" + name + "-" + service
}

func snapshotImage(name, service string) string {
	return "sbx-snap-" + name + "-" + service + ":latest"
}

// Snapshot commits every service of a sandbox to an image.
//
// It does not stop anything first. Committing a running container gives a crash-consistent
// filesystem - the same state the service would recover from after a power cut, which every
// database in this project's examples is built to survive. Stopping first would be cleaner
// and would also mean the snapshot silently interrupts whoever is using the sandbox.
func Snapshot(ctx context.Context, p provider.Provider, sandbox, name string) ([]SnapshotRef, error) {
	if err := ValidateName("sandbox", sandbox); err != nil {
		return nil, err
	}

	if err := ValidateSnapshotName(name); err != nil {
		return nil, err
	}

	if name == "" {
		return nil, fmt.Errorf("a snapshot needs a name: sbx snapshot <sandbox> <name>")
	}

	// Asked once, at the top, so a backend that cannot do this says so before anything is
	// half done rather than failing on the third service.
	snap, err := provider.SnapshotterFor(p)
	if err != nil {
		return nil, err
	}

	units, err := p.List(ctx, sandbox)
	if err != nil {
		return nil, err
	}

	if len(units) == 0 {
		return nil, UnknownSandbox(ctx, p, sandbox)
	}

	vols, _ := p.(provider.NamedVolumes)

	// Everything this call writes, so a failure can take it back. A snapshot that fails part
	// way used to leave its first images and volumes behind under a message saying nothing
	// was changed - and SnapshotsOf finds a snapshot by its images, so a fork would happily
	// start from the half that got made.
	var madeImages, madeVolumes []string

	fail := func(err error) ([]SnapshotRef, error) {
		for _, img := range madeImages {
			_ = snap.RemoveImage(ctx, img)
		}

		if vols != nil {
			for _, v := range madeVolumes {
				_ = vols.RemoveVolume(ctx, v)
			}
		}

		return nil, fmt.Errorf("%w\n\nno snapshot %q was saved: the %d image(s) and %d volume(s) "+
			"this attempt wrote were removed", err, name, len(madeImages), len(madeVolumes))
	}

	// Volumes first, then images - and the order is the point. Images are how SnapshotsOf
	// finds a snapshot, so while any volume is still being copied there is no image for a
	// fork to find, even if this process is killed before it can roll back.
	volumeOf := make(map[string]string, len(units))

	for _, u := range units {
		src := snap.VolumeFor(sandbox, u.Service)
		if src == "" {
			continue // the backend keeps no per-service volume (a microVM's disk is in its image)
		}

		// A service that declared no `volume` has none. Copying from it anyway had docker
		// create an empty source to mount, which the copy then refused, failing the whole
		// snapshot. Its state is its filesystem, which the image carries.
		if vols != nil {
			ok, err := vols.VolumeExists(ctx, src)
			if err != nil {
				return fail(fmt.Errorf("snapshotting %s's data: %w", u.Service, err))
			}

			if !ok {
				continue
			}
		}

		dst := snapshotVolume(name, u.Service)

		existed := false
		if vols != nil {
			existed, _ = vols.VolumeExists(ctx, dst)
		}

		err := snap.CopyVolume(ctx, src, dst)

		// An empty volume is the state of a service that has not written anything yet: a
		// fork starting from a fresh volume starts from exactly that, so it is image-only
		// rather than a failure. CopyVolume has already removed what its mounts created.
		if errors.Is(err, provider.ErrEmptyVolume) {
			continue
		}

		if !existed {
			madeVolumes = append(madeVolumes, dst)
		}

		if err != nil {
			return fail(fmt.Errorf("snapshotting %s's data: %w", u.Service, err))
		}

		volumeOf[u.Service] = dst
	}

	refs := make([]SnapshotRef, 0, len(units))

	for _, u := range units {
		img := snapshotImage(name, u.Service)
		dst := volumeOf[u.Service]

		// Labels say which snapshot the image belongs to - its name alone is ambiguous when
		// one snapshot's name is a prefix of another's - and whether a volume goes with it,
		// so a fork can tell "image-only" from "its volume was deleted".
		// A backend with no per-service volume takes no image-config changes either.
		var changes []string
		if snap.VolumeFor(sandbox, u.Service) != "" {
			changes = []string{
				"LABEL " + labelSnapshotName + "=" + name,
				"LABEL " + labelSnapshotVolume + "=" + cmp.Or(dst, noVolume),
			}
		}

		if err := snap.Commit(ctx, u.Ref, img, changes...); err != nil {
			return fail(fmt.Errorf("snapshotting %s: %w", u.Service, err))
		}

		madeImages = append(madeImages, img)

		if dst != "" {
			fmt.Printf("  %-12s → %s  + volume %s\n", u.Service, img, dst)
		} else {
			fmt.Printf("  %-12s → %s  (no volume)\n", u.Service, img)
		}

		refs = append(refs, SnapshotRef{Service: u.Service, Image: img, Volume: dst})
	}

	return refs, nil
}

// Image labels a snapshot writes. noVolume is recorded rather than an absent label, because an
// absent label is what every snapshot taken before these existed looks like.
const (
	labelSnapshotName   = "sbx.snapshot.name"
	labelSnapshotVolume = "sbx.snapshot.volume"
	noVolume            = "none"
)

// ForkSpec rewrites a spec so each service starts from the snapshot's image instead of the
// original one. The volume is deliberately dropped: a named volume would be shared by every
// fork, so twenty agents would be writing to one disk and the isolation the fork exists to
// provide would be a fiction. The state lives in the image now.
func ForkSpec(sp map[string]any, name string, refs []SnapshotRef) error {
	services, ok := sp["services"].(map[string]any)
	if !ok {
		return fmt.Errorf("the spec has no services")
	}

	for _, r := range refs {
		svc, ok := services[r.Service].(map[string]any)
		if !ok {
			return fmt.Errorf("the spec has no service %q to fork", r.Service)
		}

		svc["image"] = r.Image

		// The snapshot's image replaces the build, which has already happened. Left in, a spec
		// with both is refused, so a fork of any sandbox with a built service failed - after
		// its data had been restored.
		delete(svc, "build")

		// The volume STAYS. The fork gets its own, restored from the snapshot's copy after
		// creation - an earlier version deleted it on the theory that the image carried the
		// data, and the fork started blank because docker commit does not capture volumes.

		// init has already run in the state being forked. Running it again would re-seed a
		// database that is already seeded, which for anything with a unique constraint is
		// an error and for anything without one is silent duplication.
		delete(svc, "init")
	}

	return nil
}

// snapshotRefs resolves a snapshot name to its images and volumes.
//
// Images are found by the prefix sbx-snap-<name>-, which also matches every snapshot whose
// name continues past it: "qa" matches "qa-s1"'s images. A labelled image says which snapshot
// it belongs to, so those are exact. An image from before the labels is taken on the prefix
// alone, as it always was - unless strict, where a service part with a dash in it could be
// another snapshot's and is returned in ambiguous instead. Strict is for deleting.
//
// The volume label decides Volume: "none" is an image-only service, a name is the volume that
// goes with it (checked when a fork copies from it), and no label is the older convention of
// sbx-snapvol-<name>-<service>.
func snapshotRefs(ctx context.Context, snap provider.Snapshotter, name string, strict bool,
) (refs []SnapshotRef, ambiguous []string, err error) {
	images, err := snap.Images(ctx, "sbx-snap-"+name+"-")
	if err != nil {
		return nil, nil, err
	}

	labeler, _ := snap.(provider.ImageLabeler)

	read := func(img, key string) (string, error) {
		if labeler == nil {
			return "", nil
		}

		v, err := labeler.ImageLabel(ctx, img, key)
		if v == "<no value>" {
			v = ""
		}

		return strings.TrimSpace(v), err
	}

	for _, img := range images {
		service := strings.TrimSuffix(strings.TrimPrefix(img, "sbx-snap-"+name+"-"), ":latest")
		if service == "" {
			continue
		}

		owner, err := read(img, labelSnapshotName)
		if err != nil {
			return nil, nil, err
		}

		switch {
		case owner != "" && owner != name:
			continue // another snapshot's, whose name starts with this one
		case owner == "" && strict && strings.Contains(service, "-"):
			ambiguous = append(ambiguous, img)
			continue
		}

		vol, err := read(img, labelSnapshotVolume)
		if err != nil {
			return nil, nil, err
		}

		ref := SnapshotRef{Service: service, Image: img, Volume: snapshotVolume(name, service)}

		switch vol {
		case "":
		case noVolume:
			ref.Volume = ""
		default:
			ref.Volume = vol
		}

		refs = append(refs, ref)
	}

	return refs, ambiguous, nil
}

// RemoveSnapshot deletes one snapshot: its images and the volumes that go with them. The only
// other way was `sbx gc --snapshots --force`, which deletes every snapshot there is.
func RemoveSnapshot(ctx context.Context, p provider.Provider, name string) error {
	if err := ValidateSnapshotName(name); err != nil {
		return err
	}

	snap, err := provider.SnapshotterFor(p)
	if err != nil {
		return err
	}

	refs, ambiguous, err := snapshotRefs(ctx, snap, name, true)
	if err != nil {
		return err
	}

	for _, img := range ambiguous {
		fmt.Printf("  skipped %s: made before snapshots were labelled, it may belong to a "+
			"snapshot whose name starts with %q. If it is this one's: docker rmi %s\n", img, name+"-", img)
	}

	if len(refs) == 0 {
		return fmt.Errorf("no snapshot %q - sbx gc --snapshots lists the ones there are", name)
	}

	vols, _ := p.(provider.NamedVolumes)

	var failed []string

	for _, r := range refs {
		if err := snap.RemoveImage(ctx, r.Image); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", r.Image, err))
			continue
		}

		fmt.Printf("  removed %s\n", r.Image)

		// The conventional name too, not only the labelled one: a snapshot of an empty volume
		// is labelled "none" but a copy from an earlier snapshot of the same name may remain.
		for _, v := range compactVolumes(r.Volume, snapshotVolume(name, r.Service)) {
			if vols == nil {
				break
			}

			if ok, _ := vols.VolumeExists(ctx, v); !ok {
				continue
			}

			if err := vols.RemoveVolume(ctx, v); err != nil {
				failed = append(failed, fmt.Sprintf("%s: %v", v, err))
				continue
			}

			fmt.Printf("  removed %s\n", v)
		}
	}

	if len(failed) > 0 {
		return fmt.Errorf("snapshot %q was only partly removed - a fork still using an image "+
			"keeps it; remove the fork (sbx rm) and run this again:\n  %s", name, strings.Join(failed, "\n  "))
	}

	fmt.Printf("snapshot %q removed\n", name)

	return nil
}

func compactVolumes(vs ...string) []string {
	var out []string

	for _, v := range vs {
		if v != "" && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}

	return out
}

// SnapshotsOf lists the images belonging to a snapshot name, so a fork can find them again
// without being told which services existed.
func SnapshotsOf(ctx context.Context, p provider.Provider, name string) ([]SnapshotRef, error) {
	snap, err := provider.SnapshotterFor(p)
	if err != nil {
		return nil, err
	}

	refs, _, err := snapshotRefs(ctx, snap, name, false)
	if err != nil {
		return nil, err
	}

	if len(refs) == 0 {
		return nil, fmt.Errorf("no snapshot %q - run: sbx snapshot <sandbox> %s", name, name)
	}

	return refs, nil
}

// Fork creates a new sandbox from a snapshot.
//
// The rewritten spec is written next to the original rather than into a temp dir that
// disappears: `sbx env` and every later command take a --spec, and a fork whose spec cannot
// be named again is a sandbox you can create and then never address.
func Fork(ctx context.Context, p provider.Provider, specPath, snapshot, sandbox string,
	withOptional bool, iso provider.Isolation,
) error {
	// Before the snapshot lookup, not after. Create validates this too, but by then the
	// snapshot has been resolved and a temporary spec written - work thrown away for
	// something knowable from the argument itself.
	if err := ValidateName("sandbox", sandbox); err != nil {
		return err
	}

	snap, err := provider.SnapshotterFor(p)
	if err != nil {
		return err
	}

	refs, err := SnapshotsOf(ctx, p, snapshot)
	if err != nil {
		return err
	}

	body, err := os.ReadFile(specPath)
	if err != nil {
		return err
	}

	var sp map[string]any
	if err := json.Unmarshal(body, &sp); err != nil {
		return fmt.Errorf("reading %s: %w", specPath, err)
	}

	if err := ForkSpec(sp, snapshot, refs); err != nil {
		return err
	}

	out, err := json.MarshalIndent(sp, "", "  ")
	if err != nil {
		return err
	}

	// Named after the fork, not the snapshot: every fork of one snapshot would otherwise
	// write the same file, and the hint below would send the first fork to the last one's spec.
	forked := filepath.Join(filepath.Dir(specPath), "sandbox."+sandbox+".json")
	if err := os.WriteFile(forked, append(out, '\n'), 0o644); err != nil {
		return err
	}

	fmt.Printf("  spec     %s\n", forked)

	// The data goes in before anything can run on it.
	//
	// This used to be the other way round - Create, then restore - and the order was the whole
	// of the bug. Create starts every service to health-check it, so between that and the
	// restore there was a window where the fork EXISTED, its database was up, and it was
	// serving the empty data directory it had just initialised for itself. Kill `sbx fork`
	// anywhere in that window and what is left is a healthy server with the wrong data, which
	// is the one outcome a fork must never produce: interrupt-e2e states the invariant as "a
	// fork that ends up present is either correct or cleanly gone", and this failed it
	// intermittently, in CI and locally, on two different rounds.
	//
	// Filling the volumes first closes both halves of it. Interrupted here, no sandbox exists
	// yet and the volumes are orphans that `sbx gc` collects - cleanly gone. Interrupted
	// inside Create, every service that exists is already sitting on the snapshot's data -
	// correct. There is no ordering left in which a running service has data that is neither.
	//
	// It also retires the hazard the old comment was about: nothing is running while the copy
	// happens, so there is no floor to replace underneath a live process, and no Stop to do.
	if err := restoreVolumes(ctx, snap, sandbox, refs); err != nil {
		return err
	}

	if err := Create(ctx, p, forked, sandbox, withOptional, iso); err != nil {
		return err
	}

	fmt.Printf("\n  forked from snapshot %q - filesystem state only, processes start cold.\n", snapshot)
	fmt.Printf("  use it with: sbx env %s --spec %s\n", sandbox, forked)

	return nil
}

// restoreVolumes lays the snapshot's data down under the names the services are about to be
// created with.
//
// It runs BEFORE Create, which is the correctness argument and the whole reason it is its own
// function. Nothing is running yet, so there is no service to stop, no live data directory to
// overwrite, and - the part that matters - no window in which a fork exists and serves data
// that is not the snapshot's. See the comment at the call site.
//
// Docker creates a named volume on first mount, so the destination does not have to exist. A
// service in the spec that the snapshot has nothing for is simply not written to, and starts
// as any fresh service would.
func restoreVolumes(ctx context.Context, snap provider.Snapshotter,
	sandbox string, refs []SnapshotRef,
) error {
	for _, r := range refs {
		if r.Volume == "" {
			continue
		}

		if err := snap.CopyVolume(ctx, r.Volume, snap.VolumeFor(sandbox, r.Service)); err != nil {
			return fmt.Errorf("restoring %s's data: %w", r.Service, err)
		}

		fmt.Printf("  %-12s restored from %s\n", r.Service, r.Volume)
	}

	return nil
}
