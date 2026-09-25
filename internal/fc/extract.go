package fc

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// tarContentSize sums the regular files in a tar, which is what the ext4 has to hold.
func tarContentSize(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	var n int64

	tr := tar.NewReader(f)

	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return n, nil
		}

		if err != nil {
			return 0, err
		}

		// Per-entry overhead too: a directory or a symlink still costs an inode and a block.
		n += h.Size + 4096
	}
}

// extractTar unpacks an exported container filesystem into dst.
//
// Only for the mkfs that cannot read a tar itself, and only as root (see RootfsBuilder.Root),
// so owners survive. Device nodes and fifos are skipped rather than created: an exported /dev is
// empty in practice because the engine mounts over it, and PID 1 mounts devtmpfs there at boot.
//
// Every path is checked to stay under dst without passing through a symlink: an exported
// filesystem comes from an arbitrary image, and "etc -> /" followed by "etc/passwd" would
// otherwise write to the host.
func extractTar(src, dst string, chown bool) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()

	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}

	type dirMeta struct {
		path string
		h    *tar.Header
	}

	var dirs []dirMeta // modes applied last, so a read-only directory can still be filled

	tr := tar.NewReader(f)

	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return err
		}

		p, err := safeJoin(dst, h.Name)
		if err != nil {
			return err
		}

		if p == dst {
			continue
		}

		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}

		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}

			dirs = append(dirs, dirMeta{p, h})

			continue
		case tar.TypeReg:
			out, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				return err
			}

			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}

			if err := out.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			// The target is not checked: it is a string stored in the ext4 and resolved inside
			// the guest, never followed here.
			_ = os.Remove(p)

			if err := os.Symlink(h.Linkname, p); err != nil {
				return err
			}

			if chown {
				_ = os.Lchown(p, h.Uid, h.Gid)
			}

			continue
		case tar.TypeLink:
			target, err := safeJoin(dst, h.Linkname)
			if err != nil {
				return err
			}

			_ = os.Remove(p)

			if err := os.Link(target, p); err != nil {
				return err
			}

			continue
		default:
			continue // char, block, fifo: see above
		}

		if err := applyMeta(p, h, chown); err != nil {
			return err
		}
	}

	for i := len(dirs) - 1; i >= 0; i-- {
		if err := applyMeta(dirs[i].path, dirs[i].h, chown); err != nil {
			return err
		}
	}

	return nil
}

// applyMeta sets owner then mode: chown clears setuid, so the other order would lose it.
func applyMeta(p string, h *tar.Header, chown bool) error {
	if chown {
		if err := os.Lchown(p, h.Uid, h.Gid); err != nil {
			return fmt.Errorf("chown %s: %w", p, err)
		}
	}

	return os.Chmod(p, h.FileInfo().Mode()&(os.ModePerm|os.ModeSetuid|os.ModeSetgid|os.ModeSticky))
}

// safeJoin joins name under root and refuses anything that would land outside it, including
// through a symlink that an earlier entry created.
func safeJoin(root, name string) (string, error) {
	clean := filepath.Clean("/" + filepath.FromSlash(name))
	p := filepath.Join(root, clean)

	if p != root && !strings.HasPrefix(p, root+string(filepath.Separator)) {
		return "", fmt.Errorf("tar entry %q escapes the root", name)
	}

	rel, _ := filepath.Rel(root, filepath.Dir(p))
	if rel == "." {
		return p, nil
	}

	cur := root

	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)

		st, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			break
		}

		if err != nil {
			return "", err
		}

		if st.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("tar entry %q passes through the symlink %s", name, cur)
		}
	}

	return p, nil
}
