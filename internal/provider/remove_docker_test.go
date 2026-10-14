package provider

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// `sbx rm` promises to destroy a sandbox "with its data". An image can hold data somewhere sbx
// never mounted: redis:7-alpine declares VOLUME /data and postgres declares one at PGDATA, so
// docker gives every such container an anonymous volume. `docker rm -f` leaves those behind -
// measured, one per redis sandbox removed, each holding that sandbox's dump.rdb - and nothing
// ever collects them: they are not named sbx-*, so `sbx gc` does not see them either.
func TestRemoveTakesTheImagesAnonymousVolumes(t *testing.T) {
	d := dockerOrSkip(t)
	ctx := context.Background()

	sandbox := fmt.Sprintf("rmvol-%d", time.Now().UnixNano())
	ref := "sbx-" + sandbox + "-cache"

	t.Cleanup(func() { _, _ = d.docker("rm", "-f", "-v", ref) })

	// -v /data is what an image's VOLUME instruction does, without depending on one.
	if out, err := d.docker("run", "-d", "--name", ref,
		"--label", labelSandbox+"="+sandbox, "--label", labelService+"=cache",
		"--label", labelPorts+"=29999:39999",
		"-v", "/data", "alpine:3", "sleep", "300"); err != nil {
		t.Fatalf("starting %s: %v: %s", ref, err, out)
	}

	out, err := d.docker("inspect", "-f", "{{range .Mounts}}{{.Name}}{{end}}", ref)
	if err != nil {
		t.Fatalf("inspect %s: %v", ref, err)
	}

	vol := strings.TrimSpace(out)
	if vol == "" {
		t.Fatalf("%s has no anonymous volume to test with", ref)
	}

	t.Cleanup(func() { _, _ = d.docker("volume", "rm", "-f", vol) })

	if err := d.Remove(ctx, sandbox); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if _, err := d.docker("volume", "inspect", vol); err == nil {
		t.Fatalf("anonymous volume %s outlived sbx rm %s: the sandbox's data was not destroyed", vol, sandbox)
	}
}

// `docker rm -f` exits 0 for a container that does not exist, so a sandbox that never had an
// egress filter was told "removed the egress filter" on every rm - a line that sends someone
// looking for a filter they never configured.
func TestRemoveFilterContainerSaysNothingWhenThereWasNone(t *testing.T) {
	d := dockerOrSkip(t)

	if d.removeFilterContainer(fmt.Sprintf("nofilter-%d", time.Now().UnixNano())) {
		t.Fatal("reported removing an egress filter for a sandbox that never had one")
	}
}

// What a half-finished create leaves: the sandbox's bridge and its egress filter, and no service
// container at all. `sbx rm` used to refuse that outright - it looks a sandbox up by its units,
// found none, and returned "no sandbox" before it got anywhere near the filter - so the only way
// to reclaim either was raw docker.
//
// It still reports that there was no sandbox, because there was not: `sbx rm typo` must not turn
// into silent success. It removes the leftovers on the way to saying so.
func TestRemoveTakesAnOrphanedEgressFilterAndStillSaysThereWasNoSandbox(t *testing.T) {
	d := dockerOrSkip(t)
	ctx := context.Background()

	sandbox := fmt.Sprintf("rmorphan-%d", time.Now().UnixNano())
	filter, network := filterContainer(sandbox), egressNetwork(sandbox)

	t.Cleanup(func() {
		_, _ = d.docker("rm", "-f", filter)
		_, _ = d.docker("network", "rm", network)
	})

	if _, err := d.ensureEgressNetwork(sandbox); err != nil {
		t.Fatalf("making the sandbox's bridge: %v", err)
	}

	// Shaped like a filter: the sandbox label and no sbx.service, which is what tells it apart
	// from a service container.
	if out, err := d.docker("run", "-d", "--name", filter,
		"--label", labelSandbox+"="+sandbox, "--network", network,
		"alpine:3", "sleep", "300"); err != nil {
		t.Fatalf("starting %s: %v: %s", filter, err, out)
	}

	err := d.Remove(ctx, sandbox)
	if err == nil {
		t.Fatal("a sandbox with no services was reported removed; `sbx rm typo-name` must still fail")
	}

	if !strings.Contains(err.Error(), sandbox) {
		t.Errorf("the refusal does not name the sandbox: %v", err)
	}

	if _, err := d.docker("inspect", "-f", "{{.Id}}", filter); err == nil {
		t.Errorf("the orphaned egress filter %s outlived `sbx rm`, and nothing else can reach "+
			"it: it has a published port, a route out and its control token on a label", filter)
	}

	if _, err := d.docker("network", "inspect", network); err == nil {
		t.Errorf("the orphaned bridge %s outlived `sbx rm`", network)
	}
}

// And a name that was never a sandbox is still refused, with nothing removed - the reordering
// must not make `sbx rm` quietly succeed on a typo.
func TestRemoveStillRefusesASandboxThatNeverExisted(t *testing.T) {
	d := dockerOrSkip(t)

	sandbox := fmt.Sprintf("rmnever-%d", time.Now().UnixNano())

	err := d.Remove(context.Background(), sandbox)
	if err == nil {
		t.Fatal("`sbx rm` on a name that was never a sandbox reported success")
	}

	if !strings.Contains(err.Error(), sandbox) {
		t.Errorf("the refusal does not name the sandbox: %v", err)
	}
}

// The last resort. Create takes its own leftovers back when it returns an error, and `sbx rm`
// takes them on the way to reporting there was no sandbox - but neither runs for a create that
// was KILLED, and then `sbx gc` is the only thing left. It could not see either artifact.
func TestGcFindsAndReclaimsAHalfFinishedCreatesLeftovers(t *testing.T) {
	d := dockerOrSkip(t)

	sandbox := fmt.Sprintf("gcorphan-%d", time.Now().UnixNano())
	filter, network := filterContainer(sandbox), egressNetwork(sandbox)

	t.Cleanup(func() {
		_, _ = d.docker("rm", "-f", filter)
		_, _ = d.docker("network", "rm", network)
	})

	if _, err := d.ensureEgressNetwork(sandbox); err != nil {
		t.Fatalf("making the sandbox's bridge: %v", err)
	}

	if out, err := d.docker("run", "-d", "--name", filter,
		"--label", labelSandbox+"="+sandbox, "--network", network,
		"alpine:3", "sleep", "300"); err != nil {
		t.Fatalf("starting %s: %v: %s", filter, err, out)
	}

	live, err := d.liveSandboxes()
	if err != nil {
		t.Fatal(err)
	}

	if live[sandbox] {
		t.Fatalf("a sandbox whose only container is its egress filter counts as live, so its "+
			"leftovers - and its data volume - can never be reclaimed: %q", sandbox)
	}

	// Grace zero: this one is seconds old, and the point here is the listing, not the wait.
	var found []string
	for _, a := range d.egressOrphans(live, 0) {
		if a.Sandbox == sandbox {
			found = append(found, a.Kind+":"+a.Name)
		}
	}

	if len(found) != 2 {
		t.Fatalf("gc saw %v of %s's leftovers, want both the filter container and the bridge",
			found, sandbox)
	}

	// And the grace holds a fresh one back, so `sbx gc --force` cannot kill a filter a create
	// that is still running needs.
	for _, a := range d.egressOrphans(live, time.Hour) {
		if a.Sandbox == sandbox {
			t.Errorf("a leftover seconds old was offered for deletion: %+v", a)
		}
	}

	for _, a := range d.egressOrphans(live, 0) {
		if a.Sandbox != sandbox {
			continue
		}

		if err := d.Reclaim(context.Background(), a); err != nil {
			t.Errorf("Reclaim(%+v): %v", a, err)
		}
	}

	if _, err := d.docker("inspect", "-f", "{{.Id}}", filter); err == nil {
		t.Errorf("gc could not reclaim the orphaned egress filter %s", filter)
	}

	if _, err := d.docker("network", "inspect", network); err == nil {
		t.Errorf("gc could not reclaim the orphaned bridge %s", network)
	}
}
