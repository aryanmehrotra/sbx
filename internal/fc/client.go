// Package fc drives Firecracker: its REST API over a unix socket, the firecracker process that
// serves it, the pinned binary and guest kernel, and the root filesystem a VM boots from.
//
// Nothing here knows what a sandbox is. internal/provider turns a spec.Service into calls on
// this package; this package turns those into a running, snapshotted or restored VM. Keeping
// the line there means the helper-VM layer (sbx inside a nested-virtualisation Linux VM on a
// Mac) runs exactly this code, unchanged, one level down.
//
// The whole control plane is net/http with a unix DialContext. That is the constraint the
// design turns on - go.mod has no requires, so no VMM SDK and no cgo - and the spike
// (docs/superpowers/specs/2026-09-26-firecracker-spike.md) drove every call used here from
// about 150 lines of it against the pinned v1.17.0.
package fc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// Client talks to one firecracker process's API socket.
type Client struct {
	sock string
	hc   *http.Client
}

// NewClient returns a client for the API socket at sock. Nothing is dialled until a call.
func NewClient(sock string) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
		// One VMM, one socket, calls strictly in sequence: a pool buys nothing and an idle
		// keep-alive held past the process's death turns the next call's error into a
		// confusing EOF instead of a clean "connection refused".
		DisableKeepAlives: true,
	}

	return &Client{sock: sock, hc: &http.Client{Transport: tr}}
}

// Socket is the path this client dials.
func (c *Client) Socket() string { return c.sock }

// APIError is a request Firecracker answered and refused. Fault is its fault_message verbatim,
// because that sentence is the only diagnosis there is: "Invalid kernel path" or "The requested
// operation is not supported after starting the microVM" say what went wrong far better than a
// status code, and paraphrasing them loses the phrase somebody would search for.
type APIError struct {
	Method string
	Path   string
	Status int
	Fault  string
}

func (e *APIError) Error() string {
	fault := e.Fault
	if fault == "" {
		fault = "(no fault_message in the response)"
	}

	return fmt.Sprintf("firecracker %s %s: %d %s: %s", e.Method, e.Path, e.Status,
		http.StatusText(e.Status), fault)
}

// ErrUnreachable wraps a call that never got an answer: the socket is absent, or nothing is
// accepting on it. It is a different failure from APIError - the VMM is gone rather than
// unwilling - and the provider acts on the difference (a dead VMM is "asleep", not an error).
var ErrUnreachable = errors.New("firecracker API socket unreachable")

// BootSource is PUT /boot-source.
type BootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args,omitempty"`
	InitrdPath      string `json:"initrd_path,omitempty"`
}

// Drive is PUT /drives/{drive_id}.
type Drive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
}

// MachineConfig is PUT /machine-config.
type MachineConfig struct {
	VcpuCount  int `json:"vcpu_count"`
	MemSizeMib int `json:"mem_size_mib"`

	// TrackDirtyPages is what makes a Diff snapshot possible later. It costs a little on every
	// guest write, and the Stop path's 128 MiB → ~2 MiB snapshot (spike, measured) pays for it.
	TrackDirtyPages bool `json:"track_dirty_pages"`
}

// NetworkInterface is PUT /network-interfaces/{iface_id}.
type NetworkInterface struct {
	IfaceID     string `json:"iface_id"`
	HostDevName string `json:"host_dev_name"`
	GuestMAC    string `json:"guest_mac,omitempty"`
}

// Vsock is PUT /vsock. The host side is a unix socket at UDSPath: a host-initiated connection
// dials it and writes "CONNECT <port>\n" (Firecracker's hybrid vsock).
type Vsock struct {
	GuestCID int    `json:"guest_cid"`
	UDSPath  string `json:"uds_path"`
}

// SnapshotType is Full or Diff.
type SnapshotType string

const (
	SnapshotFull SnapshotType = "Full"
	SnapshotDiff SnapshotType = "Diff"
)

// SnapshotCreate is PUT /snapshot/create. The VM must be paused first.
type SnapshotCreate struct {
	SnapshotType SnapshotType `json:"snapshot_type,omitempty"`
	SnapshotPath string       `json:"snapshot_path"`
	MemFilePath  string       `json:"mem_file_path"`
}

// MemBackend is SnapshotLoad's memory source.
type MemBackend struct {
	BackendType string `json:"backend_type"` // "File" here; "Uffd" is for a page-fault server sbx does not run
	BackendPath string `json:"backend_path"`
}

// NetworkOverride points a restored interface at a different tap.
type NetworkOverride struct {
	IfaceID     string `json:"iface_id"`
	HostDevName string `json:"host_dev_name"`
}

// VsockOverride points a restored vsock device at a different host socket.
type VsockOverride struct {
	UDSPath string `json:"uds_path"`
}

// SnapshotLoad is PUT /snapshot/load, on a firecracker process that has been configured with
// nothing else.
//
// The two overrides are why N clones of one snapshot need neither a jailer nor a mount namespace
// each: the device paths baked into the snapshot state are replaced per clone at load time.
// Both are in the pinned v1.17.0 SnapshotLoadParams.
type SnapshotLoad struct {
	SnapshotPath     string            `json:"snapshot_path"`
	MemBackend       MemBackend        `json:"mem_backend"`
	TrackDirtyPages  bool              `json:"track_dirty_pages,omitempty"`
	ResumeVM         bool              `json:"resume_vm,omitempty"`
	NetworkOverrides []NetworkOverride `json:"network_overrides,omitempty"`
	VsockOverride    *VsockOverride    `json:"vsock_override,omitempty"`
}

// InstanceInfo is GET /.
type InstanceInfo struct {
	ID         string `json:"id"`
	State      string `json:"state"` // "Not started", "Running", "Paused"
	VMMVersion string `json:"vmm_version"`
	AppName    string `json:"app_name"`
}

// Instance states as Firecracker spells them.
const (
	StateNotStarted = "Not started"
	StateRunning    = "Running"
	StatePaused     = "Paused"
)

func (c *Client) PutBootSource(ctx context.Context, b BootSource) error {
	return c.do(ctx, http.MethodPut, "/boot-source", b, nil)
}

func (c *Client) PutDrive(ctx context.Context, d Drive) error {
	return c.do(ctx, http.MethodPut, "/drives/"+d.DriveID, d, nil)
}

func (c *Client) PutMachineConfig(ctx context.Context, m MachineConfig) error {
	return c.do(ctx, http.MethodPut, "/machine-config", m, nil)
}

func (c *Client) PutNetworkInterface(ctx context.Context, n NetworkInterface) error {
	return c.do(ctx, http.MethodPut, "/network-interfaces/"+n.IfaceID, n, nil)
}

func (c *Client) PutVsock(ctx context.Context, v Vsock) error {
	return c.do(ctx, http.MethodPut, "/vsock", v, nil)
}

// PutEntropy attaches a virtio-rng device, so the guest's getrandom never blocks at boot on a
// machine with nothing else to feed it.
func (c *Client) PutEntropy(ctx context.Context) error {
	return c.do(ctx, http.MethodPut, "/entropy", struct{}{}, nil)
}

// InstanceStart boots the configured VM.
func (c *Client) InstanceStart(ctx context.Context) error {
	return c.action(ctx, "InstanceStart")
}

// SendCtrlAltDel asks the guest to shut down. x86_64 only: Firecracker implements it through
// the i8042 keyboard controller, which aarch64 does not have, and refuses it there - the caller
// gets that refusal as an APIError rather than a silent no-op.
func (c *Client) SendCtrlAltDel(ctx context.Context) error {
	return c.action(ctx, "SendCtrlAltDel")
}

func (c *Client) action(ctx context.Context, kind string) error {
	return c.do(ctx, http.MethodPut, "/actions", map[string]string{"action_type": kind}, nil)
}

// Pause freezes the vCPUs. Required before a snapshot.
func (c *Client) Pause(ctx context.Context) error {
	return c.do(ctx, http.MethodPatch, "/vm", map[string]string{"state": "Paused"}, nil)
}

// Resume thaws them.
func (c *Client) Resume(ctx context.Context) error {
	return c.do(ctx, http.MethodPatch, "/vm", map[string]string{"state": "Resumed"}, nil)
}

func (c *Client) CreateSnapshot(ctx context.Context, s SnapshotCreate) error {
	return c.do(ctx, http.MethodPut, "/snapshot/create", s, nil)
}

func (c *Client) LoadSnapshot(ctx context.Context, s SnapshotLoad) error {
	return c.do(ctx, http.MethodPut, "/snapshot/load", s, nil)
}

// Describe is GET /: the instance's state and the VMM's version.
func (c *Client) Describe(ctx context.Context) (InstanceInfo, error) {
	var info InstanceInfo
	err := c.do(ctx, http.MethodGet, "/", nil, &info)

	return info, err
}

// WaitReady polls until the API socket answers, or ctx ends. A freshly spawned firecracker
// takes ~1.6 ms (spike, median) to bind it, so the poll is tight.
func (c *Client) WaitReady(ctx context.Context) error {
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()

	var last error

	for {
		if _, err := c.Describe(ctx); err == nil {
			return nil
		} else {
			last = err
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("firecracker API at %s never answered: %w (last: %v)", c.sock, ctx.Err(), last)
		case <-tick.C:
		}
	}
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader

	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}

		body = bytes.NewReader(b)
	}

	// The host part is ignored - DialContext always goes to the socket - but it must parse.
	req, err := http.NewRequestWithContext(ctx, method, "http://firecracker"+path, body)
	if err != nil {
		return err
	}

	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	req.Header.Set("Accept", "application/json")

	resp, err := c.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("firecracker %s %s: %w", method, path, ctx.Err())
		}

		return fmt.Errorf("%w: %s %s via %s: %v", ErrUnreachable, method, path, c.sock, err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode/100 != 2 {
		var fault struct {
			FaultMessage string `json:"fault_message"`
		}

		_ = json.Unmarshal(raw, &fault)

		return &APIError{Method: method, Path: path, Status: resp.StatusCode, Fault: fault.FaultMessage}
	}

	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("firecracker %s %s: unreadable response %q: %w", method, path, raw, err)
		}
	}

	return nil
}
