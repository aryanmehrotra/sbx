package fc

import (
	"context"
	"errors"
	"net"
)

// The guest seam: everything the provider needs from inside a VM, which it cannot do itself.
//
// Owned by another piece of work (branch osb/fc-vsock): execd's AF_VSOCK listener, the host
// side of Firecracker's hybrid vsock (internal/fcvsock), and the control-plane client that seals
// execd before a snapshot and re-keys it after a restore (internal/execdctl). This package does
// not reimplement any of that. It names the four operations the VM lifecycle depends on and
// ships NoGuest, which refuses each one honestly, so the provider compiles, runs and is tested
// on its own - and the integration is one type implementing Guest, assigned to NewGuest.
//
// Why the lifecycle depends on it at all: a restored snapshot is the parent's memory, including
// execd's access token and every key userspace generated before the snapshot (the spike measured
// a pre-snapshot token identical in every clone at N=2..50). So a snapshot is taken only after
// Seal, and Start does not return - the wake proxy does not let a byte through - until Rekey has
// given the restored execd its identity.

// GuestVM identifies one VM to the guest channel.
type GuestVM struct {
	Sandbox, Service string
	Dir              string // the VM's directory
	VsockUDS         string // Firecracker's hybrid-vsock host socket for this VM
}

// Rekey is the identity a restored execd is given.
type Rekey struct {
	Generation    uint64   // increases on every restore; execd refuses an older one
	AccessToken   string   // what API clients present; unchanged across a plain resume
	ControlSecret string   // authenticates Seal and Rekey themselves
	Env           []string // KEY=VALUE applied to commands execd runs from now on
}

// Guest is the channel into a VM.
type Guest interface {
	// Dial opens a byte stream to port inside the VM, over vsock.
	Dial(ctx context.Context, vm GuestVM, port int) (net.Conn, error)

	// Seal tells execd a snapshot is about to be taken: it stops answering until re-keyed.
	Seal(ctx context.Context, vm GuestVM, secret string) error

	// Rekey gives a restored execd its identity. The provider calls it before Start returns.
	Rekey(ctx context.Context, vm GuestVM, k Rekey) error

	// Available reports whether this is a real channel. NoGuest says false, and the provider
	// then skips Seal/Rekey on its own sleep/wake (the identity does not change there) and
	// refuses anything that needs them to be real: exec, copy, and forking a snapshot.
	Available() bool
}

// ErrGuestUnavailable is every NoGuest answer.
var ErrGuestUnavailable = errors.New("the Firecracker guest channel (vsock to execd) is not in " +
	"this build: exec, copy and forking a VM snapshot need it, and sbx will not fake them over " +
	"another path. Build from a tree that carries internal/fcvsock and internal/execdctl")

// NoGuest is the Guest used until a real one is wired in.
type NoGuest struct{}

func (NoGuest) Dial(context.Context, GuestVM, int) (net.Conn, error) { return nil, ErrGuestUnavailable }
func (NoGuest) Seal(context.Context, GuestVM, string) error          { return ErrGuestUnavailable }
func (NoGuest) Rekey(context.Context, GuestVM, Rekey) error          { return ErrGuestUnavailable }
func (NoGuest) Available() bool                                      { return false }

// NewGuest makes the provider's guest channel. The integration replaces it; see the file comment.
var NewGuest = func() Guest { return NoGuest{} }

// ExecdVsockPort is where execd listens inside the VM (`sbx execd --vsock-port`), the same number
// it uses over TCP in a container.
const ExecdVsockPort = 44772
