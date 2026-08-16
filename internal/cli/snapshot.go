package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

	refs := make([]SnapshotRef, 0, len(units))

	for _, u := range units {
		img := snapshotImage(name, u.Service)

		if err := snap.Commit(ctx, u.Ref, img); err != nil {
			return nil, fmt.Errorf("snapshotting %s: %w", u.Service, err)
		}

		// The part that actually carries the data.
		src := snap.VolumeFor(sandbox, u.Service)
		dst := snapshotVolume(name, u.Service)

		if src != "" {
			if err := snap.CopyVolume(ctx, src, dst); err != nil {
				return nil, fmt.Errorf("snapshotting %s's data: %w", u.Service, err)
			}
		}

		fmt.Printf("  %-12s → %s  + volume %s\n", u.Service, img, dst)

		refs = append(refs, SnapshotRef{Service: u.Service, Image: img, Volume: dst})
	}

	return refs, nil
}

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

// SnapshotsOf lists the images belonging to a snapshot name, so a fork can find them again
// without being told which services existed.
func SnapshotsOf(ctx context.Context, p provider.Provider, name string) ([]SnapshotRef, error) {
	snap, err := provider.SnapshotterFor(p)
	if err != nil {
		return nil, err
	}

	images, err := snap.Images(ctx, "sbx-snap-"+name+"-")
	if err != nil {
		return nil, err
	}

	refs := make([]SnapshotRef, 0, len(images))

	for _, img := range images {
		service := strings.TrimSuffix(strings.TrimPrefix(img, "sbx-snap-"+name+"-"), ":latest")
		if service == "" {
			continue
		}

		refs = append(refs, SnapshotRef{
			Service: service, Image: img, Volume: snapshotVolume(name, service),
		})
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

	forked := filepath.Join(filepath.Dir(specPath), "sandbox."+snapshot+".json")
	if err := os.WriteFile(forked, append(out, '\n'), 0o644); err != nil {
		return err
	}

	fmt.Printf("  spec     %s\n", forked)

	if err := Create(ctx, p, forked, sandbox, withOptional, iso); err != nil {
		return err
	}

	// Restore after creation, and with the service stopped. Create starts each service to
	// health-check it, which for a database means it initialised an empty data directory;
	// writing over that while it is running would be replacing the floor underneath it.
	units, err := p.List(ctx, sandbox)
	if err != nil {
		return err
	}

	if err := restoreVolumes(ctx, p, snap, sandbox, units, refs); err != nil {
		return err
	}

	fmt.Printf("\n  forked from snapshot %q - filesystem state only, processes start cold.\n", snapshot)
	fmt.Printf("  use it with: sbx env %s --spec %s\n", sandbox, forked)

	return nil
}

// restoreVolumes copies each snapshotted volume over the freshly created service's own.
//
// Separated from Fork for one reason: the order here is the correctness argument, and it is
// the only part of a fork that can silently produce a healthy server with the wrong data.
// Create has just started every service to health-check it, so a database is running and
// serving from a data directory it initialised itself. Copying over that while it runs is
// replacing the floor underneath a live process.
//
// So: stop, then copy, per service - never copy first, and never copy into a service that is
// still up. Anything not present in the snapshot is left exactly as Create made it.
func restoreVolumes(ctx context.Context, p provider.Provider, snap provider.Snapshotter,
	sandbox string, units []provider.Unit, refs []SnapshotRef,
) error {
	for _, u := range units {
		var from string

		for _, r := range refs {
			if r.Service == u.Service {
				from = r.Volume
			}
		}

		if from == "" {
			continue
		}

		if err := p.Stop(ctx, u.Ref); err != nil {
			return fmt.Errorf("stopping %s before restoring its data: %w", u.Service, err)
		}

		if err := snap.CopyVolume(ctx, from, snap.VolumeFor(sandbox, u.Service)); err != nil {
			return fmt.Errorf("restoring %s's data: %w", u.Service, err)
		}

		fmt.Printf("  %-12s restored from %s\n", u.Service, from)
	}

	return nil
}
