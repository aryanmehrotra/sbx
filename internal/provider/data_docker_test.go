package provider

// The docker paths that move or replace a service's state: re-creating a service whose image
// changed, restoring a checkpoint, and the empty-source refusal in CopyVolume. Each of these
// once reported success for something that had not happened, which is why they are exercised
// against a real engine rather than a fake - the lie was in what docker did, not in our logic.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/spec"
)

// stdoutOf runs f and returns what it printed. Create reports "already exists" and "recreated"
// on stdout, and that line is the only thing telling a user which of the two happened.
func stdoutOf(t *testing.T, f func()) string {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	old := os.Stdout
	os.Stdout = w

	defer func() { os.Stdout = old }()

	f()

	_ = w.Close()

	out, _ := io.ReadAll(r)

	return string(out)
}

func imageOf(t *testing.T, d *dockerProvider, ref string) string {
	t.Helper()

	out, err := d.docker("inspect", "--format", "{{.Config.Image}}", ref)
	if err != nil {
		t.Fatalf("inspect %s: %v", ref, err)
	}

	return strings.TrimSpace(out)
}

// Re-running `sbx create` after a `build` context changed builds sbx-build-<newhash> and then
// found the container already there, so the service kept running the OLD image while create
// printed a check mark. The declared image is the one thing a re-create must honour; the data
// volume is the one thing it must keep.
func TestCreateRecreatesAServiceWhoseImageChanged(t *testing.T) {
	d := dockerOrSkip(t)
	ctx := context.Background()

	sandbox := fmt.Sprintf("recreate-%d", time.Now().UnixNano())
	ref := containerName(sandbox, "app")

	t.Cleanup(func() {
		_, _ = d.docker("rm", "-f", ref)
		_, _ = d.docker("volume", "rm", "-f", volumeName(sandbox, "app"))
	})

	svc := spec.Service{Image: "alpine:3", Volume: "/data", Args: []string{"sleep", "300"}}

	if err := d.Create(ctx, sandbox, 0, 0, "app", svc, nil, t.TempDir(), IsolationContainer); err != nil {
		t.Fatalf("first create: %v", err)
	}

	if out, err := d.docker("exec", ref, "sh", "-c", "echo kept > /data/marker"); err != nil {
		t.Fatalf("seeding the volume: %v: %s", err, out)
	}

	firstID, _ := d.docker("inspect", "--format", "{{.Id}}", ref)

	// Same image: nothing is replaced, and the message says so.
	out := stdoutOf(t, func() {
		if err := d.Create(ctx, sandbox, 0, 0, "app", svc, nil, t.TempDir(), IsolationContainer); err != nil {
			t.Fatalf("second create, same image: %v", err)
		}
	})

	if id, _ := d.docker("inspect", "--format", "{{.Id}}", ref); id != firstID {
		t.Fatalf("an unchanged image still replaced the container (%s -> %s)", firstID, id)
	}

	if !strings.Contains(out, "already exists") {
		t.Errorf("an unchanged service did not say it already exists: %q", out)
	}

	// A new image: the container is replaced and says why.
	// Asked for with a different tier, as a bare `sbx create` (default container) is on a gVisor
	// sandbox: the replacement keeps the tier the sandbox was made with, not the flag default.
	svc.Image = "busybox:1.36"

	out = stdoutOf(t, func() {
		if err := d.Create(ctx, sandbox, 0, 0, "app", svc, nil, t.TempDir(), IsolationGVisor); err != nil {
			t.Fatalf("create with a new image: %v", err)
		}
	})

	if got := imageOf(t, d, ref); got != "busybox:1.36" {
		t.Fatalf("the service still runs %s after its declared image became busybox:1.36 - "+
			"create reported success on the old build", got)
	}

	if got, _ := d.docker("inspect", "--format", label(labelIsolation), ref); got != string(IsolationContainer) {
		t.Errorf("the re-created service changed tier to %q; it must keep the sandbox's", got)
	}

	if !strings.Contains(out, "recreated (image changed)") {
		t.Errorf("the replacement was silent: %q", out)
	}

	if got, err := d.docker("exec", ref, "cat", "/data/marker"); err != nil || strings.TrimSpace(got) != "kept" {
		t.Fatalf("the data volume did not survive the re-create: %q %v", got, err)
	}
}

// Every container carries its isolation tier, so `sbx add` can put a new service on the same
// runtime as the rest of the sandbox instead of silently on runc.
func TestCreateLabelsTheIsolationTier(t *testing.T) {
	d := dockerOrSkip(t)
	ctx := context.Background()

	sandbox := fmt.Sprintf("isolabel-%d", time.Now().UnixNano())

	t.Cleanup(func() { _, _ = d.docker("rm", "-f", containerName(sandbox, "app")) })

	slot, err := d.AllocSlot(ctx, sandbox)
	if err != nil {
		t.Fatal(err)
	}

	svc := spec.Service{Image: "alpine:3", Ports: []int{80}, Args: []string{"sleep", "300"}}
	eps := d.Endpoints(sandbox, "app", slot, 0, svc.Ports)

	if err := d.Create(ctx, sandbox, slot, 0, "app", svc, eps, t.TempDir(), IsolationContainer); err != nil {
		t.Fatalf("create: %v", err)
	}

	units, err := d.List(ctx, sandbox)
	if err != nil || len(units) != 1 {
		t.Fatalf("List = %v, %v", units, err)
	}

	if units[0].Isolation != IsolationContainer {
		t.Fatalf("unit isolation = %q, want %q: the tier was not recorded", units[0].Isolation, IsolationContainer)
	}
}

func TestUnitOfReadsTheIsolationLabel(t *testing.T) {
	c := container{ID: "x", Names: []string{"/sbx-a-b"}, State: "exited", Labels: map[string]string{
		labelSandbox: "a", labelService: "b", labelPorts: "20000:40000", labelIsolation: "gvisor",
	}}

	u, ok := unitOf(c)
	if !ok || u.Isolation != IsolationGVisor {
		t.Fatalf("unitOf = %+v %v, want isolation gvisor", u, ok)
	}

	delete(c.Labels, labelIsolation)

	if u, _ := unitOf(c); u.Isolation != "" {
		t.Fatalf("an unlabelled (older) unit reported isolation %q; it should report none", u.Isolation)
	}
}

// `docker start --checkpoint` on a container that is already running exits 0 and does
// nothing, so `sbx resume` printed "resumed - memory and processes intact" about a process
// that was never restored. A running service was woken after its checkpoint, so the checkpoint
// is stale; the only honest answer is a refusal.
func TestRestoreRefusesARunningContainer(t *testing.T) {
	d := dockerOrSkip(t)

	ref := fmt.Sprintf("sbx-restore-running-%d", time.Now().UnixNano())

	t.Cleanup(func() { _, _ = d.docker("rm", "-f", ref) })

	if out, err := d.docker("run", "-d", "--name", ref, "alpine:3", "sleep", "300"); err != nil {
		t.Fatalf("starting %s: %v: %s", ref, err, out)
	}

	err := d.Restore(context.Background(), ref, "cp1")
	if err == nil {
		t.Fatal("restoring into a running container reported success; nothing was restored")
	}

	if !strings.Contains(err.Error(), "sbx sleep") {
		t.Errorf("the refusal does not say what to do next (sbx sleep): %v", err)
	}
}

// On macOS the engine is in a VM. With experimental on, `docker checkpoint create` succeeds
// there and only the restore fails (a netns bind-mount error), so the checkpoint is one that
// can never be resumed. Refused before anything is dumped, and without asking docker at all.
func TestCheckpointIsRefusedOffLinux(t *testing.T) {
	old := hostOS
	hostOS = "darwin"

	t.Cleanup(func() { hostOS = old })

	d := &dockerProvider{endpoint: dockerEndpoint{Network: "unix", Address: "/nonexistent/docker.sock"}}

	err := d.checkpointReady()
	if err == nil {
		t.Fatal("a memory checkpoint was allowed on macOS, where it can never be resumed")
	}

	for _, want := range []string{"macOS", "Linux", "sbx snapshot"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

// "the source is empty or does not exist - nothing was changed" was false: `docker run -v`
// creates both named volumes before the script can look, so the refusal left an empty source
// and an empty destination behind. The message is only true if the call removes what it made.
func TestCopyVolumeFromNothingLeavesNothing(t *testing.T) {
	d := dockerOrSkip(t)
	ctx := context.Background()

	src := fmt.Sprintf("sbx-test-nosrc-%d", time.Now().UnixNano())
	dst := src + "-dst"

	t.Cleanup(func() {
		_, _ = d.docker("volume", "rm", "-f", src)
		_, _ = d.docker("volume", "rm", "-f", dst)
	})

	err := d.CopyVolume(ctx, src, dst)
	if !errors.Is(err, ErrEmptyVolume) {
		t.Fatalf("CopyVolume from a missing volume = %v, want ErrEmptyVolume", err)
	}

	for _, v := range []string{src, dst} {
		if ok, _ := d.VolumeExists(ctx, v); ok {
			t.Errorf("volume %s was left behind by a copy that said nothing was changed", v)
		}
	}
}

// A destination that already existed is the caller's and is not removed on refusal.
func TestCopyVolumeKeepsADestinationItDidNotCreate(t *testing.T) {
	d := dockerOrSkip(t)
	ctx := context.Background()

	src := fmt.Sprintf("sbx-test-nosrc2-%d", time.Now().UnixNano())
	dst := src + "-dst"

	volume(t, d, dst, 1)
	t.Cleanup(func() { _, _ = d.docker("volume", "rm", "-f", src) })

	if err := d.CopyVolume(ctx, src, dst); !errors.Is(err, ErrEmptyVolume) {
		t.Fatalf("CopyVolume = %v, want ErrEmptyVolume", err)
	}

	if n := countFiles(t, d, dst); n != 1 {
		t.Fatalf("the pre-existing destination holds %d files after a refused copy, want 1", n)
	}
}

// The port inside the container each backing port reaches, read from what docker publishes, so
// a readiness check can ask the container itself whether anything listens there. A stopped
// container publishes nothing, and says so with a 0 rather than a guess.
func TestUnitOfReadsThePortInsideTheContainer(t *testing.T) {
	c := container{ID: "x", Names: []string{"/sbx-a-b"}, State: "running", Labels: map[string]string{
		labelSandbox: "a", labelService: "b", labelPorts: "20000:40000,20001:40001",
	}, Ports: []containerPort{
		{PrivatePort: 9000, PublicPort: 40001, Type: "tcp"},
		{PrivatePort: 6379, PublicPort: 40000, Type: "tcp"},
		{PrivatePort: 6379, PublicPort: 40000, Type: "tcp"}, // docker lists v4 and v6 bindings separately
	}}

	u, _ := unitOf(c)
	if len(u.Private) != 2 || u.Private[0] != 6379 || u.Private[1] != 9000 {
		t.Fatalf("Private = %v, want [6379 9000] in Upstream order", u.Private)
	}

	c.State, c.Ports = "exited", nil

	if u, _ := unitOf(c); len(u.Private) != 2 || u.Private[0] != 0 || u.Private[1] != 0 {
		t.Fatalf("a stopped container reported Private = %v, want [0 0]", u.Private)
	}
}
