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
