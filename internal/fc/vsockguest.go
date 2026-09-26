package fc

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/aryanmehrotra/sbx/internal/execdctl"
	"github.com/aryanmehrotra/sbx/internal/fcvsock"
)

// VsockGuest is the real guest channel: every operation is a connection through the VM's
// Firecracker vsock device (fcvsock), and Seal and Rekey are execd's control endpoints on it
// (execdctl). It holds nothing between calls - the device path is in GuestVM - so one value
// serves every VM and every sbx process, which is what a provider that never assumes it started
// what it is looking at needs.
type VsockGuest struct {
	// Timeout bounds one dial (connect plus handshake); zero is fcvsock.DefaultTimeout.
	Timeout time.Duration
}

var _ Guest = VsockGuest{}

// Dial opens a stream to port inside the VM. Only a guest AF_VSOCK listener answers - execd on
// ExecdVsockPort - not the workload's TCP ports: those are reached over the VM's tap.
func (g VsockGuest) Dial(ctx context.Context, vm GuestVM, port int) (net.Conn, error) {
	if port < 1 || int64(port) > 0xFFFFFFFE {
		return nil, fmt.Errorf("vsock port %d is out of range (1..4294967294)", port)
	}

	return fcvsock.Dialer{UDSPath: vm.VsockUDS, Port: uint32(port), Timeout: g.Timeout, Unix: DialVMM}.DialContext(ctx)
}

func (g VsockGuest) control(vm GuestVM) execdctl.Client {
	return execdctl.Client{Dial: func(ctx context.Context) (net.Conn, error) {
		return g.Dial(ctx, vm, ExecdVsockPort)
	}}
}

// Seal makes execd refuse every client until it is re-keyed; the snapshot then captures it
// sealed, so a restore that is never re-keyed answers nobody rather than answering as its parent.
func (g VsockGuest) Seal(ctx context.Context, vm GuestVM, secret string) error {
	if err := g.control(vm).Seal(ctx, secret); err != nil {
		return fmt.Errorf("%s/%s: %w", vm.Sandbox, vm.Service, err)
	}

	return nil
}

// Rekey gives a restored execd its identity, authorised by k.Secret and replacing it with
// k.ControlSecret. Resending the same Rekey is a no-op success on execd's side, so a caller whose
// response was lost may retry it as is.
func (g VsockGuest) Rekey(ctx context.Context, vm GuestVM, k Rekey) error {
	r := execdctl.Rekey{Generation: k.Generation, AccessToken: k.AccessToken, ControlSecret: k.ControlSecret}

	// nil stays nil: execd reads it as "leave the env alone", where an empty map would clear
	// every key a claim or an earlier re-key set.
	if k.Env != nil {
		r.Envs = make(map[string]string, len(k.Env))

		for _, kv := range k.Env {
			key, val, ok := strings.Cut(kv, "=")
			if !ok || key == "" {
				return fmt.Errorf("re-key env entry %q is not KEY=VALUE", kv)
			}

			r.Envs[key] = val
		}
	}

	if err := g.control(vm).Rekey(ctx, k.Secret, r); err != nil {
		return fmt.Errorf("%s/%s: %w", vm.Sandbox, vm.Service, err)
	}

	return nil
}

// Available is true: this is the channel, not a stand-in for it.
func (VsockGuest) Available() bool { return true }
