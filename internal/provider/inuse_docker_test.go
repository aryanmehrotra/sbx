package provider

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
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
// read: a template reading .Id beside `index .Config.Labels` fails the whole inspect on one
// (docker 29.5.2), and the image looked absent.
func TestImageMetasReadsAnImageWithNoLabels(t *testing.T) {
	d := dockerOrSkip(t)

	if _, err := d.docker("image", "inspect", VolumeCopyImage); err != nil {
		t.Skipf("no %s here", VolumeCopyImage)
	}

	if m := d.imageMetas([]string{VolumeCopyImage}); m[VolumeCopyImage].ID == "" {
		t.Fatalf("imageMetas(%s) = %v, want its ID", VolumeCopyImage, m)
	}
}

// A snapshot image made before snapshots were labelled has no labels, and SnapshotsOf and
// RemoveSnapshot fall back to its name only if ImageLabel answers "" rather than an error.
// ImageLabel's template indexes the labels and reads nothing else, which docker 29.5.2 answers
// with "" on such an image (a template that also reads .Id is the one that fails; see
// imageMetas). This pins that, so a change to the template that breaks it goes red here.
func TestImageLabelOfAnUnlabelledImage(t *testing.T) {
	d := dockerOrSkip(t)
	ctx := context.Background()

	img := fmt.Sprintf("sbx-snap-legacy-test-%d-x:latest", os.Getpid())
	src := fmt.Sprintf("sbx-legacy-test-%d", os.Getpid())

	if _, err := d.docker("create", "--name", src, VolumeCopyImage, "true"); err != nil {
		t.Skipf("cannot create from %s: %v", VolumeCopyImage, err)
	}

	_, err := d.docker("commit", src, img) // no --change: no labels, as before labels existed
	_, _ = d.docker("rm", "-f", src)

	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _, _ = d.docker("rmi", img) })

	got, err := d.ImageLabel(ctx, img, SnapshotNameLabel)
	if err != nil || got != "" {
		t.Fatalf("ImageLabel of an unlabelled image = %q, %v; want \"\", nil", got, err)
	}

	// And a labelled one still reads.
	lab := strings.Replace(img, "-x:latest", "-y:latest", 1)

	if _, err := d.docker("create", "--name", src, VolumeCopyImage, "true"); err != nil {
		t.Fatal(err)
	}

	_, err = d.docker("commit", "--change", "LABEL "+SnapshotNameLabel+"=legacy", src, lab)
	_, _ = d.docker("rm", "-f", src)

	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _, _ = d.docker("rmi", lab) })

	if got, err := d.ImageLabel(ctx, lab, SnapshotNameLabel); err != nil || got != "legacy" {
		t.Fatalf("ImageLabel of a labelled image = %q, %v; want legacy", got, err)
	}
}

// A snapshot volume's label is read back, and an unlabelled one (made before snapshot volumes
// carried one) answers "" rather than an error, so gc falls back to `docker volume rm`.
func TestVolumeLabelOnDocker(t *testing.T) {
	d := dockerOrSkip(t)
	ctx := context.Background()

	labelled := fmt.Sprintf("sbx-snapvol-vollabel-test-%d-web-ui", os.Getpid())
	bare := labelled + "-bare"

	t.Cleanup(func() { _, _ = d.docker("volume", "rm", "-f", labelled, bare) })

	if err := d.CreateVolume(ctx, labelled, map[string]string{SnapshotNameLabel: "vollabel-test"}); err != nil {
		t.Fatal(err)
	}

	if _, err := d.docker("volume", "create", bare); err != nil {
		t.Fatal(err)
	}

	if got, err := d.VolumeLabel(ctx, labelled, SnapshotNameLabel); err != nil || got != "vollabel-test" {
		t.Errorf("labelled volume: %q, %v; want vollabel-test", got, err)
	}

	if got, err := d.VolumeLabel(ctx, bare, SnapshotNameLabel); err != nil || got != "" {
		t.Errorf("unlabelled volume: %q, %v; want \"\", nil", got, err)
	}
}
