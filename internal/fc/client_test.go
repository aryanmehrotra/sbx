package fc

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/fc/fcfake"
)

// sockDir is short on purpose: a unix socket path is capped at 104 bytes on macOS, and
// t.TempDir under /var/folders/... plus a test name runs past it.
func sockDir(t *testing.T) string {
	t.Helper()

	d, err := os.MkdirTemp("", "fc")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { os.RemoveAll(d) })

	return d
}

func fake(t *testing.T) (*Client, *fcfake.Server, string) {
	t.Helper()

	dir := sockDir(t)

	s, err := fcfake.Start(filepath.Join(dir, "api.sock"))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { s.Close() })

	return NewClient(filepath.Join(dir, "api.sock")), s, dir
}

func TestEveryCallReachesItsPath(t *testing.T) {
	c, s, dir := fake(t)
	ctx := context.Background()

	must := func(err error) {
		t.Helper()

		if err != nil {
			t.Fatal(err)
		}
	}

	must(c.WaitReady(ctx))
	must(c.PutBootSource(ctx, BootSource{KernelImagePath: "/k", BootArgs: "console=ttyS0"}))
	must(c.PutDrive(ctx, Drive{DriveID: "rootfs", PathOnHost: "/r.ext4", IsRootDevice: true}))
	must(c.PutMachineConfig(ctx, MachineConfig{VcpuCount: 1, MemSizeMib: 128, TrackDirtyPages: true}))
	must(c.PutNetworkInterface(ctx, NetworkInterface{IfaceID: "eth0", HostDevName: "sbxfc1", GuestMAC: "06:00:00:00:00:01"}))
	must(c.PutVsock(ctx, Vsock{GuestCID: 3, UDSPath: "/v.sock"}))
	must(c.PutEntropy(ctx))
	must(c.InstanceStart(ctx))
	must(c.SendCtrlAltDel(ctx))
	must(c.Pause(ctx))
	must(c.CreateSnapshot(ctx, SnapshotCreate{SnapshotType: SnapshotFull,
		SnapshotPath: filepath.Join(dir, "vm.state"), MemFilePath: filepath.Join(dir, "vm.mem")}))
	must(c.Resume(ctx))

	info, err := c.Describe(ctx)
	must(err)

	if info.State != StateRunning || info.VMMVersion != "1.17.0" {
		t.Fatalf("describe = %+v", info)
	}

	want := []string{
		"GET /", "PUT /boot-source", "PUT /drives/rootfs", "PUT /machine-config",
		"PUT /network-interfaces/eth0", "PUT /vsock", "PUT /entropy", "PUT /actions", "PUT /actions",
		"PATCH /vm", "PUT /snapshot/create", "PATCH /vm", "GET /",
	}
	if got := s.Paths(); !slices.Equal(got, want) {
		t.Fatalf("paths:\n got %v\nwant %v", got, want)
	}

	// The wire names are Firecracker's, not Go's: a field serialised under the wrong key is
	// silently ignored by the VMM, which is how a Diff snapshot would quietly become impossible.
	calls := s.Calls()
	if calls[3].Body["track_dirty_pages"] != true || calls[3].Body["mem_size_mib"] != float64(128) {
		t.Fatalf("machine-config body = %v", calls[3].Body)
	}

	if calls[7].Body["action_type"] != "InstanceStart" || calls[8].Body["action_type"] != "SendCtrlAltDel" {
		t.Fatalf("actions = %v, %v", calls[7].Body, calls[8].Body)
	}

	if calls[9].Body["state"] != "Paused" || calls[11].Body["state"] != "Resumed" {
		t.Fatalf("vm state bodies = %v, %v", calls[9].Body, calls[11].Body)
	}

	if calls[10].Body["snapshot_type"] != "Full" {
		t.Fatalf("snapshot body = %v", calls[10].Body)
	}
}

func TestLoadCarriesOverrides(t *testing.T) {
	// Make a snapshot on one fake, load it on another: the second process is the clone.
	c, _, dir := fake(t)
	ctx := context.Background()

	for _, err := range []error{
		c.PutBootSource(ctx, BootSource{KernelImagePath: "/k"}),
		c.PutNetworkInterface(ctx, NetworkInterface{IfaceID: "eth0", HostDevName: "tap-parent"}),
		c.PutVsock(ctx, Vsock{GuestCID: 3, UDSPath: "/parent.vsock"}),
		c.InstanceStart(ctx),
		c.Pause(ctx),
		c.CreateSnapshot(ctx, SnapshotCreate{SnapshotPath: filepath.Join(dir, "vm.state"), MemFilePath: filepath.Join(dir, "vm.mem")}),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}

	c2, s2, _ := fake(t)

	err := c2.LoadSnapshot(ctx, SnapshotLoad{
		SnapshotPath:     filepath.Join(dir, "vm.state"),
		MemBackend:       MemBackend{BackendType: "File", BackendPath: filepath.Join(dir, "vm.mem")},
		ResumeVM:         true,
		NetworkOverrides: []NetworkOverride{{IfaceID: "eth0", HostDevName: "tap-clone"}},
		VsockOverride:    &VsockOverride{UDSPath: "/clone.vsock"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if s2.Iface("eth0")["host_dev_name"] != "tap-clone" || s2.VsockPath() != "/clone.vsock" {
		t.Fatalf("overrides not applied: iface=%v vsock=%q", s2.Iface("eth0"), s2.VsockPath())
	}

	if s2.State() != "Running" {
		t.Fatalf("resume_vm did not resume: %s", s2.State())
	}

	body := s2.Calls()[0].Body
	if mb := body["mem_backend"].(map[string]any); mb["backend_type"] != "File" {
		t.Fatalf("mem_backend = %v", mb)
	}
}

func TestFaultMessageIsCarried(t *testing.T) {
	c, s, _ := fake(t)
	ctx := context.Background()

	s.Fail["/boot-source"] = "Invalid kernel path"

	err := c.PutBootSource(ctx, BootSource{KernelImagePath: "/nope"})

	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("not an APIError: %v", err)
	}

	if ae.Status != 400 || ae.Fault != "Invalid kernel path" || ae.Method != "PUT" || ae.Path != "/boot-source" {
		t.Fatalf("error = %+v", ae)
	}

	if !strings.Contains(err.Error(), "Invalid kernel path") {
		t.Fatalf("message lost the fault: %v", err)
	}

	// The fake's own rule, as the real VMM's: configuration after boot is refused.
	_ = c.PutBootSource(ctx, BootSource{KernelImagePath: "/k"})
	_ = c.InstanceStart(ctx)

	err = c.PutMachineConfig(ctx, MachineConfig{VcpuCount: 2, MemSizeMib: 256})
	if !errors.As(err, &ae) || !strings.Contains(ae.Fault, "after starting") {
		t.Fatalf("post-boot config = %v", err)
	}

	// And a snapshot of a running VM.
	err = c.CreateSnapshot(ctx, SnapshotCreate{SnapshotPath: "/x", MemFilePath: "/y"})
	if !errors.As(err, &ae) || !strings.Contains(ae.Fault, "pause") {
		t.Fatalf("snapshot while running = %v", err)
	}
}

func TestErrorWithoutFaultStillSaysSomething(t *testing.T) {
	dir := sockDir(t)
	sock := filepath.Join(dir, "api.sock")

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}

	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			_, _ = w.Write([]byte("not json"))
			return
		}

		w.WriteHeader(500)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { srv.Close() })

	c := NewClient(sock)

	err = c.InstanceStart(context.Background())

	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 500 || !strings.Contains(err.Error(), "no fault_message") {
		t.Fatalf("500 with no body = %v", err)
	}

	if _, err := c.Describe(context.Background()); err == nil || !strings.Contains(err.Error(), "unreadable response") {
		t.Fatalf("garbage body = %v", err)
	}
}

func TestUnreachableIsItsOwnError(t *testing.T) {
	c := NewClient(filepath.Join(sockDir(t), "absent.sock"))

	err := c.Pause(context.Background())
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("absent socket = %v", err)
	}

	var ae *APIError
	if errors.As(err, &ae) {
		t.Fatal("an unreachable VMM was reported as a refusal")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	if err := c.WaitReady(ctx); err == nil || !strings.Contains(err.Error(), "never answered") {
		t.Fatalf("WaitReady on nothing = %v", err)
	}
}
