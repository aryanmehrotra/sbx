package procid

import (
	"encoding/binary"
	"syscall"
	"unsafe"
)

// StartOf reads the process's start time from the kern.proc.pid sysctl: kinfo_proc begins with
// kp_proc.p_un.__p_starttime, a timeval. Asked by MIB through __sysctl, because syscall.Sysctl
// resolves names and "kern.proc.pid.<n>" is not one it resolves. Nothing is executed - `ps -o
// lstart` would answer the same at the cost of a process per question, to one-second precision.
func StartOf(pid int) (int64, bool) {
	mib := [4]int32{1, 14, 1, int32(pid)} // CTL_KERN, KERN_PROC, KERN_PROC_PID, pid

	buf := make([]byte, 1024) // sizeof(struct kinfo_proc) is 648 on arm64 and amd64
	n := uintptr(len(buf))

	_, _, errno := syscall.Syscall6(syscall.SYS___SYSCTL,
		uintptr(unsafe.Pointer(&mib[0])), uintptr(len(mib)),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n)), 0, 0)
	if errno != 0 || n < 16 {
		return 0, false // n == 0: no such process
	}

	sec := int64(binary.LittleEndian.Uint64(buf[0:8]))
	usec := int64(binary.LittleEndian.Uint32(buf[8:12]))

	if sec <= 0 {
		return 0, false
	}

	return sec*1_000_000 + usec, true
}
