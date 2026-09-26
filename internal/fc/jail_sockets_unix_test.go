//go:build unix

package fc

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/execdctl"
	"github.com/aryanmehrotra/sbx/internal/fc/fcfake"
)

// jailFor prepares a jail under a short /tmp directory (sun_path is 104 bytes on darwin) and
// returns the VM directory and its root.
func jailFor(t *testing.T) (dir, root string) {
	t.Helper()

	dir, err := os.MkdirTemp("/tmp", "fcx")
	must(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	bin := filepath.Join(dir, "firecracker")
	must(t, os.WriteFile(bin, []byte("#!"), 0o755))

	root, err = PrepareJail(LaunchSpec{Binary: bin, Dir: dir, Jail: &JailSpec{UID: os.Getuid(), GID: os.Getgid()}})
	must(t, err)

	return dir, root
}

// A compromised VMM owns its root, so it can replace its /api.sock with a symlink to another VM's
// (the path is predictable). sbx reaches the socket through <dir>/api.sock -> <root>/api.sock, and
// following that second hop would send the next Pause or CreateSnapshot to another tenant's VMM.
func TestAPlantedAPISocketSymlinkIsNeverFollowed(t *testing.T) {
	dir, root := jailFor(t)
	other, _ := jailFor(t)

	victim, err := fcfake.Start(filepath.Join(other, "victim.sock"))
	must(t, err)
	t.Cleanup(func() { _ = victim.Close() })

	must(t, os.Symlink(filepath.Join(other, "victim.sock"), filepath.Join(root, APISockName)))

	_, err = NewClient(filepath.Join(dir, APISockName)).Describe(context.Background())
	if !errors.Is(err, ErrForeignSocket) {
		t.Fatalf("Describe through a planted symlink = %v, want ErrForeignSocket", err)
	}

	if errors.Is(err, ErrUnreachable) {
		t.Fatal("a hijacked socket reads as an absent VMM, which the provider takes for asleep")
	}

	if calls := victim.Calls(); len(calls) != 0 {
		t.Fatalf("the other VM's VMM was called: %v", calls)
	}

	// The real thing - a socket the VMM bound in its own root - is dialled as before.
	must(t, os.Remove(filepath.Join(root, APISockName)))

	own, err := fcfake.Start(filepath.Join(root, APISockName))
	must(t, err)
	t.Cleanup(func() { _ = own.Close() })

	if _, err := NewClient(filepath.Join(dir, APISockName)).Describe(context.Background()); err != nil {
		t.Fatalf("Describe of the VM's own jailed VMM: %v", err)
	}

	// Nothing there at all is still an absent VMM - asleep - not a hijack.
	must(t, own.Close())
	_ = os.Remove(filepath.Join(root, APISockName))

	if _, err := NewClient(filepath.Join(dir, APISockName)).Describe(context.Background()); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("Describe with no socket = %v, want ErrUnreachable", err)
	}
}

// The same for the vsock device: a re-key carries the caller's token and env, and must never reach
// the execd of whichever VM a planted /vsock.sock names.
func TestAPlantedVsockSymlinkIsNeverFollowed(t *testing.T) {
	dir, root := jailFor(t)
	other, _ := jailFor(t)

	got := make(chan string, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("POST "+execdctl.PathRekey, func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Get(execdctl.SecretHeader)
		w.WriteHeader(http.StatusNoContent)
	})

	victim := filepath.Join(other, "victim.sock")
	newFakeDeviceAt(t, victim, ExecdVsockPort, mux)
	must(t, os.Symlink(victim, filepath.Join(root, VsockName)))

	err := VsockGuest{}.Rekey(context.Background(), GuestVM{Dir: dir, VsockUDS: filepath.Join(dir, VsockName)},
		Rekey{Secret: "s", Generation: 2, AccessToken: "caller-token", ControlSecret: "next"})
	if !errors.Is(err, ErrForeignSocket) {
		t.Fatalf("a re-key through a planted vsock symlink = %v, want ErrForeignSocket", err)
	}

	select {
	case s := <-got:
		t.Fatalf("the other VM's execd was re-keyed (secret %q)", s)
	default:
	}
}
