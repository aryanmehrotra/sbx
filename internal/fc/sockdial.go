package fc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// ErrForeignSocket is a VMM socket sbx refused to dial because it is not the one that VM's VMM
// bound: a jailed VMM owns its root, and a compromised one can leave a symlink there to another
// VM's socket. Distinct from ErrUnreachable, which the provider reads as "asleep".
var ErrForeignSocket = errors.New("refused a firecracker socket that is not its own VMM's")

// DialVMM connects to a VMM's unix socket by the path every host-side caller uses: <dir>/api.sock
// or <dir>/vsock.sock.
//
// Unjailed, that is the socket itself, bound by a root VMM in a directory only root can write.
// Jailed, it is sbx's own symlink (PrepareJail) to the same name in the VM's jail root - and that
// root belongs to the VM's uid, so what is AT that name is the VMM's to choose. A compromised one
// could put a symlink there to another VM's socket, whose path is predictable, and the next Pause
// or CreateSnapshot would go to another tenant. So sbx's link must point into this VM's own jail,
// and the entry it names in the root must be a socket (Lstat: not a symlink, not anything else)
// owned by the root's owner - the jail's uid; and once connected (through the short link, for
// sun_path's sake), the process that answers must be that uid too (peerIs: SO_PEERCRED on Linux),
// which a swap between the check and the connect cannot fake.
func DialVMM(ctx context.Context, sock string) (net.Conn, error) {
	var d net.Dialer

	st, err := os.Lstat(sock)
	if err != nil || st.Mode()&os.ModeSymlink == 0 {
		return d.DialContext(ctx, "unix", sock)
	}

	target, err := os.Readlink(sock)
	if err != nil {
		return nil, err
	}

	jails := filepath.Join(filepath.Dir(sock), JailDirName) + string(filepath.Separator)
	if !filepath.IsAbs(target) || filepath.Clean(target) != target || !strings.HasPrefix(target, jails) ||
		filepath.Base(target) != filepath.Base(sock) {
		return nil, fmt.Errorf("%w: %s points at %s, not into its own VM's jail", ErrForeignSocket, sock, target)
	}

	root := filepath.Dir(target)

	rst, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}

	if !rst.IsDir() {
		return nil, fmt.Errorf("%w: the jail root %s is a %s", ErrForeignSocket, root, rst.Mode().Type())
	}

	uid, known := fileOwner(rst)

	fst, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return d.DialContext(ctx, "unix", sock) // no VMM bound it: unreachable, as it always was
	} else if err != nil {
		return nil, err
	}

	if fst.Mode().Type() != os.ModeSocket {
		return nil, fmt.Errorf("%w: %s in the VMM's jail is a %s, not the socket it binds - the VM's VMM "+
			"may be compromised", ErrForeignSocket, target, fst.Mode().Type())
	}

	if owner, ok := fileOwner(fst); known && ok && owner != uid {
		return nil, fmt.Errorf("%w: %s in the VMM's jail belongs to uid %d, not the jail's %d",
			ErrForeignSocket, target, owner, uid)
	}

	// Through sbx's own link, not the target: the 108-byte sun_path limit applies to the path
	// given to connect, and the jail's is long. The peer check below is what binds the connection
	// to the jail's uid whatever the path resolved to by then.
	c, err := d.DialContext(ctx, "unix", sock)
	if err != nil {
		return nil, err
	}

	if known {
		if err := peerIs(c, uid); err != nil {
			_ = c.Close()
			return nil, fmt.Errorf("%w: %s: %v", ErrForeignSocket, target, err)
		}
	}

	return c, nil
}
