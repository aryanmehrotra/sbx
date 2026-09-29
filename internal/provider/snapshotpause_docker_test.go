package provider

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// The primitives `sbx snapshot` now uses, in its order, against a container that churns its
// volume the way ClickHouse's merges do: new part directories written, old ones removed, with no
// pause between. Copying that live failed ("can't stat ... No such file or directory") or tore;
// paused, the copy and the commit see one instant, and the container carries on afterwards.
func TestSnapshotPrimitivesOnAPausedChurningWriter(t *testing.T) {
	d := dockerOrSkip(t)
	ctx := context.Background()

	id := fmt.Sprintf("snappause-test-%d", os.Getpid())
	ctr, vol, dst, img := "sbx-"+id, "sbx-"+id+"-data", "sbx-snapvol-"+id, "sbx-snap-"+id+"-w:latest"

	t.Cleanup(func() {
		_, _ = d.docker("rm", "-f", ctr)
		_, _ = d.docker("volume", "rm", "-f", vol, dst)
		_, _ = d.docker("rmi", img)
	})

	churn := `i=0; while true; do mkdir -p /v/p$i; for j in 1 2 3 4 5 6 7 8; do echo $i > /v/p$i/f$j; done; ` +
		`rm -rf /v/p$((i-4)); i=$((i+1)); done`

	if _, err := d.docker("run", "-d", "--name", ctr, "-v", vol+":/v", VolumeCopyImage, "sh", "-c", churn); err != nil {
		t.Skipf("cannot run %s: %v", VolumeCopyImage, err)
	}

	time.Sleep(time.Second) // let it build up parts to churn

	// Unpaused, as before the fix: logged rather than asserted, because a race does not fail on
	// every run - but when it does, it is the RC2 failure, and the log says how often.
	torn := 0

	for range 5 {
		if err := d.CopyVolume(ctx, vol, dst); err != nil {
			torn++
		}
	}

	t.Logf("unpaused: %d of 5 copies of the churning volume failed", torn)

	for round := range 5 {
		if err := d.Pause(ctx, ctr); err != nil {
			t.Fatal(err)
		}

		copyErr := d.CopyVolume(ctx, vol, dst)
		commitErr := d.Commit(ctx, ctr, img, "LABEL "+SnapshotNameLabel+"="+id)

		if err := d.Unpause(ctx, ctr); err != nil {
			t.Fatal(err)
		}

		if copyErr != nil || commitErr != nil {
			t.Fatalf("round %d, paused: copy %v, commit %v", round, copyErr, commitErr)
		}
	}

	// Thawed and still writing: the pause was not a stop.
	state, err := d.docker("inspect", "--format", "{{.State.Status}}", ctr)
	if err != nil || strings.TrimSpace(state) != "running" {
		t.Fatalf("after the snapshots the writer is %q (%v), want running", state, err)
	}
}
