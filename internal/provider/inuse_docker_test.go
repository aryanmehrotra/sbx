package provider

import (
	"context"
	"fmt"
	"os"
	"slices"
	"testing"
)

// InUse against a real docker: a STOPPED container still holds its image and its volume, which
// is the state of every sleeping fork. By image ID, so the tag spelling does not matter.
func TestInUseSeesAStoppedContainer(t *testing.T) {
	d := dockerOrSkip(t)
	ctx := context.Background()

	id := fmt.Sprintf("inuse-test-%d", os.Getpid())
	img := "sbx-snap-" + id + "-x:latest"
	vol := "sbx-snapvol-" + id + "-x"
	ctr := "sbx-" + id

	// Committed, not tagged: a tag of alpine:3 shares its ID, and every container on this
	// machine created from alpine:3 would then rightly count as using it.
	if _, err := d.docker("create", "--name", ctr+"-src", VolumeCopyImage, "true"); err != nil {
		t.Skipf("cannot create from %s: %v", VolumeCopyImage, err)
	}

	_, err := d.docker("commit", "--change", "LABEL "+SnapshotNameLabel+"="+id, ctr+"-src", img)
	_, _ = d.docker("rm", "-f", ctr+"-src")

	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		_, _ = d.docker("rm", "-f", ctr)
		_, _ = d.docker("volume", "rm", "-f", vol)
		_, _ = d.docker("rmi", img)
	})

	used, err := d.InUse(ctx, []string{img}, []string{vol})
	if err != nil {
		t.Fatal(err)
	}

	if len(used) != 0 {
		t.Fatalf("nothing uses them yet, InUse = %v", used)
	}

	// Created, never started: no running process, as a fork asleep.
	if _, err := d.docker("create", "--name", ctr, "--label", labelSandbox+"="+id,
		"-v", vol+":/data", "sbx-snap-"+id+"-x", "true"); err != nil {
		t.Fatal(err)
	}

	used, err = d.InUse(ctx, []string{img}, []string{vol})
	if err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(used[img], []string{id}) || !slices.Equal(used[vol], []string{id}) {
		t.Fatalf("InUse = %v, want both used by sandbox %s", used, id)
	}
}

// An image with no labels at all (alpine:3, or a snapshot from before labels) still has to be
// read: `index .Config.Labels` failed the whole inspect on one, and the image looked absent.
func TestImageMetasReadsAnImageWithNoLabels(t *testing.T) {
	d := dockerOrSkip(t)

	if _, err := d.docker("image", "inspect", VolumeCopyImage); err != nil {
		t.Skipf("no %s here", VolumeCopyImage)
	}

	if m := d.imageMetas([]string{VolumeCopyImage}); m[VolumeCopyImage].ID == "" {
		t.Fatalf("imageMetas(%s) = %v, want its ID", VolumeCopyImage, m)
	}
}
