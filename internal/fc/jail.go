package fc

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The jailer: every Firecracker VMM runs under Firecracker's own `jailer` (pinned with it, from
// the same release tarball), which chroots it into a directory holding only what that VM needs,
// drops it to a uid of its own, and puts it in a cgroup v2 sized from the spec. What it replaces
// is v0.12's accepted risk - the VMM as unconfined root, so a guest that escaped into it had the
// host (SECURITY.md).
//
// The interface is v1.17.0's, read from its source (src/jailer/src/env.rs) and docs/jailer.md at
// that tag:
//
//	jailer --id <id> --exec-file <fc> --uid <u> --gid <g> --chroot-base-dir <base>
//	       [--cgroup-version 2 --parent-cgroup <p> --cgroup cpu.max=... --cgroup memory.max=...]
//	       -- <firecracker args>
//
// It chroots into <base>/<basename of the canonical exec file>/<id>/root, which it creates and
// chowns to the uid; copies the binary in; mknods /dev/kvm, /dev/net/tun, /dev/urandom and
// /dev/userfaultfd (failing EEXIST on any a previous run left, so each launch starts from an empty
// root); creates <cgroup2 mount>/<parent>/<id>, enabling the controllers on the way down; and
// execs firecracker as `--id <id> --start-time-us ... <args>` in the SAME process (no
// --new-pid-ns, no --daemonize), so the pid sbx started is the VMM's and its stdout and stderr
// are still the console and vmm log files sbx opened.
//
// Layout. The jail lives INSIDE the VM's directory - <dir>/jail/<exec>/<id>/root - so everything
// that already removes a VM's directory removes its jail, and the per-VM files are hard links
// within one filesystem. Every path the VMM is given is relative to that root: the kernel at
// /vmlinux, drives at /rootfs.ext4, /agent.ext4 and /vol<N>.ext4, the snapshot at /vm.state and
// /vm.mem, sockets at /api.sock and /vsock.sock. <dir>/api.sock and <dir>/vsock.sock are symlinks
// into the root, so every host-side caller (the API client, the vsock dialer, the wake proxy)
// keeps the path it always used, and the 108-byte unix socket limit applies to that short path,
// not to the chroot's.
const (
	// JailerEnv turns the jailer off: SBX_FC_JAILER=off. For a development host where the jailer
	// cannot run (no cgroup v2 delegation, a filesystem that refuses mknod); never the default,
	// and warned about on every use (SECURITY.md).
	JailerEnv = "SBX_FC_JAILER"

	// JailUIDBaseEnv moves the uid range: SBX_FC_JAILER_UID_BASE. The range is
	// [base, base+JailUIDSpan) and must not hold a real account.
	JailUIDBaseEnv = "SBX_FC_JAILER_UID_BASE"

	DefaultJailUIDBase = 900000

	// JailUIDSpan is the range the uid arithmetic covers: 254 slots of 256 indexes.
	JailUIDSpan = 254 * 256

	JailDirName = "jail"

	// JailCgroupParent is where each VM's cgroup is made: <cgroup2>/sbx-fc/<jail id>.
	JailCgroupParent = "sbx-fc"

	// JailMemOverheadMiB is what memory.max allows on top of the guest's memory: the VMM's own
	// heap and the page cache of its drive and snapshot I/O, which the cgroup is charged for and
	// reclaims under the limit rather than OOM-killing the VM.
	JailMemOverheadMiB = 128
)

// JailConfig is the jailer turned on.
type JailConfig struct {
	UIDBase int
}

// JailFromEnv reads SBX_FC_JAILER and SBX_FC_JAILER_UID_BASE: nil is the jailer off. A value it
// cannot read is an error, not either answer - a typo in an escape hatch must not open it.
func JailFromEnv(getenv func(string) string) (*JailConfig, error) {
	switch v := strings.ToLower(strings.TrimSpace(getenv(JailerEnv))); v {
	case "", "on", "1", "true":
	case "off", "0", "false":
		return nil, nil
	default:
		return nil, fmt.Errorf("%s=%q: on (the default) or off", JailerEnv, v)
	}

	c := &JailConfig{UIDBase: DefaultJailUIDBase}

	if v := strings.TrimSpace(getenv(JailUIDBaseEnv)); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 || n+JailUIDSpan > 1<<32-2 {
			return nil, fmt.Errorf("%s=%q: a uid above 0 with %d free above it", JailUIDBaseEnv, v, JailUIDSpan)
		}

		c.UIDBase = int(n)
	}

	return c, nil
}

// UID is the uid (and gid) a VM's VMM runs as: base + slot*256 + index. Arithmetic on the address,
// like the IP: every sbx process computes the same one without a table, a re-created VM gets its
// own back, and two VMs on the host never share one - so a VMM cannot signal, ptrace or open the
// files of another, and never runs as root.
func (c JailConfig) UID(a Addr) int { return c.UIDBase + a.Slot*256 + a.Index }

// JailID is the jailer's --id for the VM in dir, which also names its cgroup: derived from the
// whole directory, so two state roots holding the same ref do not share a cgroup.
func JailID(dir string) string {
	h := sha256.Sum256([]byte(filepath.Clean(dir)))
	return "sbx-" + hex.EncodeToString(h[:8])
}

// JailRoot is where the jailer chroots the VMM launched from binary in dir: its
// <chroot base>/<exec file name>/<id>/root, the name taken as the jailer takes it, from the
// canonical path.
func JailRoot(dir, binary string) string {
	if p, err := filepath.EvalSymlinks(binary); err == nil {
		binary = p
	}

	return filepath.Join(dir, JailDirName, filepath.Base(binary), JailID(dir), "root")
}

// JailSpec is a LaunchSpec's jail: set, the VMM is started through the jailer.
type JailSpec struct {
	Jailer   string // the jailer binary
	UID, GID int    // JailConfig.UID of the VM's address; never 0

	// CPUs and MemMiB become cpu.max and memory.max. Both 0 is no cgroup at all.
	CPUs, MemMiB int

	// Files are put in the root before the VMM starts: everything it will be told to open.
	Files []Stage
}

// Stage is one file the VMM is given, at /<Name> in its root.
type Stage struct {
	Name string
	Host string

	// Shared: not this VM's (the kernel). Never re-owned; hard-linked only when any user may
	// read it, and copied otherwise. A VM's own file is hard-linked and given to its uid, so
	// what the guest writes to its disk is on the VM's disk.
	Shared bool
}

// JailerArgs is the jailer's argv (without argv[0]) for s.
func JailerArgs(s LaunchSpec) []string {
	j := s.Jail

	args := []string{
		"--id", JailID(s.Dir),
		"--exec-file", s.Binary,
		"--uid", strconv.Itoa(j.UID), "--gid", strconv.Itoa(j.GID),
		"--chroot-base-dir", filepath.Join(s.Dir, JailDirName),
	}

	var limits []string
	if j.CPUs > 0 {
		limits = append(limits, "--cgroup", fmt.Sprintf("cpu.max=%d 100000", j.CPUs*100000))
	}

	if j.MemMiB > 0 {
		limits = append(limits, "--cgroup", "memory.max="+strconv.Itoa((j.MemMiB+JailMemOverheadMiB)<<20))
	}

	// Only with a limit: v1.17 with --cgroup-version 2 and no --cgroup MOVES the process into an
	// existing --parent-cgroup, which fails once any VM has enabled memory in its subtree.
	if len(limits) > 0 {
		args = append(args, "--cgroup-version", "2", "--parent-cgroup", JailCgroupParent)
		args = append(args, limits...)
	}

	return append(args, "--", "--api-sock", "/"+APISockName)
}

// PrepareJail empties the VM's jail and fills its root with s's files, owned as they must be, and
// points <dir>/api.sock and <dir>/vsock.sock into it. It returns the root.
//
// Emptied first because the jailer's mknod of /dev/kvm fails EEXIST on the one a previous run
// left, and because the previous run's hard links would keep a replaced snapshot's blocks
// allocated. The caller has ended any VMM in dir.
func PrepareJail(s LaunchSpec) (string, error) {
	j := s.Jail

	if j.UID <= 0 && os.Geteuid() == 0 {
		return "", fmt.Errorf("the jailed VMM for %s would run as uid %d; a jail's uid is never root", s.Dir, j.UID)
	}

	if err := os.RemoveAll(filepath.Join(s.Dir, JailDirName)); err != nil {
		return "", fmt.Errorf("clearing the previous jail: %w", err)
	}

	root := JailRoot(s.Dir, s.Binary)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}

	for _, f := range j.Files {
		if err := stage(root, f, j.UID, j.GID); err != nil {
			return "", fmt.Errorf("putting %s in the VMM's jail: %w", f.Host, err)
		}
	}

	for _, name := range []string{APISockName, VsockName} {
		link := filepath.Join(s.Dir, name)
		if err := os.Remove(link); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}

		if err := os.Symlink(filepath.Join(root, name), link); err != nil {
			return "", err
		}
	}

	return root, nil
}

func stage(root string, f Stage, uid, gid int) error {
	if f.Name == "" || strings.ContainsRune(f.Name, '/') {
		return fmt.Errorf("%q is not a file name", f.Name)
	}

	st, err := os.Stat(f.Host)
	if err != nil {
		return err
	}

	if !st.Mode().IsRegular() {
		return errors.New("not a regular file")
	}

	dst := filepath.Join(root, f.Name)

	if f.Shared {
		// Linked only when the VMM could read the original anyway: re-owning a shared file would
		// hand it to one VM, and a link to a root-only one would be unreadable in the jail.
		if st.Mode().Perm()&0o004 != 0 && os.Link(f.Host, dst) == nil {
			return nil
		}

		if _, err := CloneFile(f.Host, dst); err != nil {
			return err
		}

		return own(dst, uid, gid)
	}

	// A VM's own drive or snapshot must be the SAME file: a copy would be a disk the guest
	// writes and the host never sees. So a link or nothing - the jail is inside the VM's
	// directory, on its filesystem, and so is every file the VM owns.
	if err := os.Link(f.Host, dst); err != nil {
		return fmt.Errorf("hard-linking into the jail (it must be on the same filesystem as the "+
			"VM's directory): %w", err)
	}

	return own(dst, uid, gid)
}

// View is how a VMM sees the host's files. The zero View is an unjailed VMM, which is given the
// host's own paths; a jailed one is given paths in its root, and writes its snapshots there.
type View struct {
	Root string // the jail's root; "" when unjailed
}

// At is the path to give the VMM for the host file host, staged in the jail under name.
func (v View) At(host, name string) string {
	if v.Root == "" {
		return host
	}

	return "/" + name
}

// Path is At under the host file's own name.
func (v View) Path(host string) string { return v.At(host, filepath.Base(host)) }

// Adopt moves what the jailed VMM wrote at /<name> to host, and keeps it only if it is a plain
// file with no other name: the VMM owns its root, so a compromised one could leave a symlink or a
// hard link to a host file there, which the host would then read, merge into or clone as root.
// Checked after the rename, in a directory only root can write, so the VMM cannot swap it after
// the check. Unjailed, the VMM wrote host itself, and there is nothing to do.
func (v View) Adopt(host, name string) error {
	if v.Root == "" {
		return nil
	}

	if err := os.Rename(filepath.Join(v.Root, name), host); err != nil {
		return fmt.Errorf("taking %s from the VMM's jail: %w", name, err)
	}

	st, err := os.Lstat(host)
	if err != nil {
		return err
	}

	if !st.Mode().IsRegular() || linkCount(st) != 1 {
		_ = os.RemoveAll(host)

		return fmt.Errorf("the jailed VMM left %s as something other than a plain file of its own "+
			"(%s); refused - the VM's VMM may be compromised", name, st.Mode().Type())
	}

	// Back to the owner of the directory it now lives in: a uid is reused by whatever VM next
	// takes the address.
	_ = os.Chown(host, os.Geteuid(), os.Getegid())

	return nil
}

// own gives path to uid:gid. Only root can give a file away, and only root can run the jailer:
// an unprivileged caller (a test preparing a root it will not jail into) keeps it as it is.
func own(path string, uid, gid int) error {
	if os.Geteuid() != 0 && uid != os.Getuid() {
		return nil
	}

	return os.Chown(path, uid, gid)
}
