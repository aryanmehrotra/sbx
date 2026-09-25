package fc

import (
	"context"
	"errors"
	"net"
)

// The guest seam: everything the provider needs from inside a VM, which it cannot do itself.
//
// The pieces live elsewhere: execd's AF_VSOCK listener (internal/execd), the host side of
// Firecracker's hybrid vsock (internal/fcvsock), and the control-plane client that seals execd
// before a snapshot and re-keys it after a restore (internal/execdctl). VsockGuest
// (vsockguest.go) joins them into one Guest and is the default wherever the provider drives
// Firecracker itself; NoGuest, which refuses each operation honestly, is what every other
// platform gets, so nothing there pretends to reach a VM it cannot.
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
	// Secret is the control secret execd holds NOW - the one captured in the snapshot - and
	// authorises the call. ControlSecret replaces it and must differ: a secret every clone
	// shares is dead the moment each is re-keyed.
	Secret string

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
var ErrGuestUnavailable = errors.New("the Firecracker guest channel (vsock to execd) is only " +
	"wired where sbx drives Firecracker itself (linux with /dev/kvm; on a Mac, inside the helper " +
	"VM): exec, copy and forking a VM snapshot need it, and sbx will not fake them over another path")

// NoGuest is the Guest used until a real one is wired in.
type NoGuest struct{}

func (NoGuest) Dial(context.Context, GuestVM, int) (net.Conn, error) { return nil, ErrGuestUnavailable }
func (NoGuest) Seal(context.Context, GuestVM, string) error          { return ErrGuestUnavailable }
func (NoGuest) Rekey(context.Context, GuestVM, Rekey) error          { return ErrGuestUnavailable }
func (NoGuest) Available() bool                                      { return false }

// NewGuest makes the provider's guest channel: VsockGuest on linux, where the provider drives
// Firecracker itself (vsockguest_linux.go), and NoGuest everywhere else.
var NewGuest = func() Guest { return NoGuest{} }

// ExecdVsockPort is where execd listens inside the VM (`sbx execd --vsock-port`), the same number
// it uses over TCP in a container.
const ExecdVsockPort = 44772
