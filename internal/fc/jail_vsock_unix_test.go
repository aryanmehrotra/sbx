//go:build unix

package fc

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/execdctl"
)

// A warm-pool claim re-keys a restored VM over its vsock device (v0.13: fcpool on the jailer).
// Jailed, the VMM binds that device at /vsock.sock in its root, and the host reaches it only
// through <dir>/vsock.sock, the symlink PrepareJail leaves. This dials the re-key through exactly
// that symlink, to a device bound at the root's path, and proves it arrives.
func TestARekeyReachesAJailedDeviceThroughTheSymlink(t *testing.T) {
	// Short, under /tmp: the device's own path in the root is long, and a bind there must fit
	// sun_path (104 bytes on darwin) in this test even though the real VMM binds "/vsock.sock".
	dir, err := os.MkdirTemp("/tmp", "fcj")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	bin := filepath.Join(dir, "firecracker")
	if err := os.WriteFile(bin, []byte("#!"), 0o755); err != nil {
		t.Fatal(err)
	}

	root, err := PrepareJail(LaunchSpec{Binary: bin, Dir: dir, Jail: &JailSpec{UID: 900001, GID: 900001}})
	if err != nil {
		t.Fatal(err)
	}

	got := make(chan string, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("POST "+execdctl.PathRekey, func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Get(execdctl.SecretHeader)
		w.WriteHeader(http.StatusNoContent)
	})

	newFakeDeviceAt(t, filepath.Join(root, VsockName), ExecdVsockPort, mux)

	link := filepath.Join(dir, VsockName)
	if st, err := os.Lstat(link); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s is not the symlink into the jail: %v", link, err)
	}

	err = VsockGuest{}.Rekey(context.Background(), GuestVM{Dir: dir, VsockUDS: link},
		Rekey{Secret: "member-secret", Generation: 2, AccessToken: "caller-token", ControlSecret: "next"})
	if err != nil {
		t.Fatalf("re-key through %s: %v", link, err)
	}

	if s := <-got; s != "member-secret" {
		t.Fatalf("execd saw secret %q, want the member's current one", s)
	}
}
