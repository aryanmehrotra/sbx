package guestinit

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"unsafe"

	"github.com/aryanmehrotra/sbx/internal/fc"
)

// Main runs as PID 1. On success it never returns: it becomes execd.
func Main(_ []string) int {
	if os.Getpid() != 1 {
		return fail("runs only as PID 1 inside a Firecracker VM (the provider puts it there)")
	}

	// CONFIG_DEVTMPFS_MOUNT=y mounts /dev before init in the pinned kernel; EBUSY is that.
	for _, m := range []struct{ src, dst, typ string }{
		{"devtmpfs", "/dev", "devtmpfs"},
		{"proc", "/proc", "proc"},
	} {
		if err := syscall.Mount(m.src, m.dst, m.typ, 0, ""); err != nil && !errors.Is(err, syscall.EBUSY) {
			return fail("mount " + m.dst + ": " + err.Error())
		}
	}

	raw, err := os.ReadFile("/init.json")
	if err != nil {
		return fail("reading /init.json: " + err.Error())
	}

	var cfg fc.InitConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fail("parsing /init.json: " + err.Error())
	}

	root := "/newroot"

	if err := syscall.Mount(cfg.RootDevice, root, "ext4", 0, ""); err != nil {
		return fail("mounting the image rootfs " + cfg.RootDevice + ": " + err.Error())
	}

	for _, d := range []string{"proc", "sys", "dev", "tmp", "opt/sbx"} {
		_ = os.MkdirAll(filepath.Join(root, d), 0o755)
	}

	for _, m := range []struct {
		src, dst, typ, data string
		flags               uintptr
	}{
		{"proc", "proc", "proc", "", syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC},
		{"sysfs", "sys", "sysfs", "", syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC},
		{"devtmpfs", "dev", "devtmpfs", "", syscall.MS_NOSUID},
	} {
		if err := syscall.Mount(m.src, filepath.Join(root, m.dst), m.typ, m.flags, m.data); err != nil {
			return fail("mount " + m.dst + ": " + err.Error())
		}
	}

	// A private devpts and a ptmx that points into it: execd's PTY sessions open /dev/ptmx.
	_ = os.MkdirAll(filepath.Join(root, "dev/pts"), 0o755)
	_ = os.MkdirAll(filepath.Join(root, "dev/shm"), 0o1777)

	if err := syscall.Mount("devpts", filepath.Join(root, "dev/pts"), "devpts",
		syscall.MS_NOSUID|syscall.MS_NOEXEC, "newinstance,ptmxmode=0666,mode=0620"); err != nil {
		return fail("mount devpts: " + err.Error())
	}

	_ = syscall.Mount("shm", filepath.Join(root, "dev/shm"), "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV, "mode=1777")
	_ = os.Remove(filepath.Join(root, "dev/ptmx"))
	_ = os.Symlink("pts/ptmx", filepath.Join(root, "dev/ptmx"))

	// The agent, read-only, at the path the docker provider also uses.
	target := filepath.Join(root, fc.GuestAgentPath)
	if f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY, 0o755); err == nil {
		f.Close()
	}

	if err := syscall.Mount("/sbx", target, "", syscall.MS_BIND, ""); err != nil {
		return fail("bind-mounting the agent: " + err.Error())
	}

	_ = syscall.Mount("", target, "", syscall.MS_BIND|syscall.MS_REMOUNT|syscall.MS_RDONLY, "")

	// Extra drives (OpenSandbox pvc volumes), into the image root before it becomes /.
	for _, st := range drivePlan(root, cfg.Mounts) {
		if err := doMount(st); err != nil {
			return fail("mounting " + st.Source + " at " + st.Target + ": " + err.Error())
		}
	}

	// eth0 was configured by the kernel (ip= on the command line); lo was not, and a workload
	// that talks to itself on 127.0.0.1 - most databases' own health checks - needs it.
	if err := loopbackUp(); err != nil {
		return fail("bringing up lo: " + err.Error())
	}

	if cfg.Hostname != "" {
		_ = syscall.Sethostname([]byte(cfg.Hostname))
	}

	// switch_root: move the image root over / and chroot into it. The agent drive stays
	// mounted underneath, unreachable, which costs nothing: it is a disk, not RAM.
	if err := os.Chdir(root); err != nil {
		return fail(err.Error())
	}

	if err := syscall.Mount(".", "/", "", syscall.MS_MOVE, ""); err != nil {
		return fail("moving the image root over /: " + err.Error())
	}

	if err := syscall.Chroot("."); err != nil {
		return fail("chroot: " + err.Error())
	}

	writeHostname("/etc/hostname", cfg.Hostname)
	writeHosts("/etc/hosts", cfg.Hostname)

	wd := cfg.WorkingDir
	if wd == "" {
		wd = "/"
	}

	if err := os.Chdir(wd); err != nil {
		return fail("working directory " + wd + ": " + err.Error())
	}

	args := execdArgs(fc.GuestAgentPath, strconv.Itoa(fc.ExecdVsockPort), cfg.Argv)

	err = syscall.Exec(fc.GuestAgentPath, args, environ(cfg.Env))

	return fail("exec execd: " + err.Error())
}

// loopbackUp is `ip link set lo up` with two ioctls, since no image can be assumed to carry
// `ip` and netlink by hand is far more than this needs.
func loopbackUp() error {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)

	var ifr [40]byte // struct ifreq: name[16], then a union whose first member is flags

	copy(ifr[:], "lo")

	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.SIOCGIFFLAGS, uintptr(unsafe.Pointer(&ifr[0]))); e != 0 {
		return e
	}

	flags := (*uint16)(unsafe.Pointer(&ifr[16]))
	*flags |= syscall.IFF_UP

	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.SIOCSIFFLAGS, uintptr(unsafe.Pointer(&ifr[0]))); e != 0 {
		return e
	}

	return nil
}

// doMount carries out one planned step.
func doMount(st mountStep) error {
	if st.MkdirAll != "" {
		if err := os.MkdirAll(st.MkdirAll, 0o755); err != nil {
			return err
		}
	}

	var flags uintptr

	switch {
	case st.Remount:
		flags = syscall.MS_BIND | syscall.MS_REMOUNT | syscall.MS_RDONLY
	case st.Bind:
		flags = syscall.MS_BIND
	case st.ReadOnly:
		flags = syscall.MS_RDONLY
	}

	return syscall.Mount(st.Source, st.Target, st.FSType, flags, "")
}
