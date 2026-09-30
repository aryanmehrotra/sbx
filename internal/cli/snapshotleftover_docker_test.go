package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// The shape `kill -9` of `sbx snapshot` mid-copy leaves, against a real docker: a snapshot
// volume with data in it and no image. `sbx snapshot --rm` finds it by name and removes it.
func TestRemoveSnapshotOfOnlyAVolumeOnDocker(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped under -short: this uses a real docker")
	}

	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("no docker daemon here")
	}

	p, err := provider.For("docker", "", "")
	if err != nil {
		t.Skipf("no docker provider: %v", err)
	}

	name := fmt.Sprintf("leftover-test-%d", os.Getpid())
	vol := "sbx-snapvol-" + name + "-db"

	docker := func(args ...string) (string, error) {
		out, err := exec.Command("docker", args...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}

	t.Cleanup(func() { _, _ = docker("volume", "rm", "-f", vol) })

	if out, err := docker("run", "--rm", "-v", vol+":/v", provider.VolumeCopyImage, "sh", "-c", "echo data > /v/f"); err != nil {
		t.Skipf("cannot write the volume: %v: %s", err, out)
	}

	if err := RemoveSnapshot(context.Background(), p, name); err != nil {
		t.Fatalf("--rm of a volume-only snapshot: %v", err)
	}

	if out, _ := docker("volume", "ls", "-q", "--filter", "name="+vol); strings.TrimSpace(out) != "" {
		t.Fatalf("the volume is still there: %q", out)
	}
}
