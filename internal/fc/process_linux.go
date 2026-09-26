package fc

import (
	"bytes"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// detached puts firecracker in its own session, so a Ctrl-C on the `sbx create` that started
// it - delivered to the whole foreground process group - does not take the VM down with it.
func detached() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }

// ownsPID reports whether pid is a live process whose command line names sock. /proc is the
// only place that answers "is this still MY firecracker" without a pidfd held from birth,
// which a VM started by another sbx process never gave us.
func ownsPID(pid int, sock string) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return false
	}

	return bytes.Contains(b, []byte(sock))
}

func killPID(pid int) error {
	err := syscall.Kill(pid, syscall.SIGKILL)
	if err == syscall.ESRCH {
		return nil
	}

	return err
}

// procStart is pid's start time (field 22 of /proc/<pid>/stat, clock ticks since boot): with the
// pid, the identity of one process that a reused pid cannot fake.
func procStart(pid int) uint64 {
	st, ok := procStat(pid)
	if !ok {
		return 0
	}

	return st.start
}

type procStatus struct {
	state   byte
	threads int
	start   uint64
}

func procStat(pid int) (procStatus, bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return procStatus{}, false
	}

	return parseProcStat(string(b))
}

// parseProcStat reads state (3), num_threads (20) and starttime (22). The command name (2) is in
// parentheses and may itself contain spaces and parentheses, so fields are counted from the LAST ')'.
func parseProcStat(s string) (procStatus, bool) {
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return procStatus{}, false
	}

	f := strings.Fields(s[i+1:]) // f[0] is field 3
	if len(f) < 20 {
		return procStatus{}, false
	}

	threads, err1 := strconv.Atoi(f[17])
	start, err2 := strconv.ParseUint(f[19], 10, 64)

	if err1 != nil || err2 != nil || len(f[0]) != 1 {
		return procStatus{}, false
	}

	return procStatus{state: f[0][0], threads: threads, start: start}, true
}

// holding reports whether the process pid (started at start; 0 means any) may still hold what it
// opened - its tap above all. Not the command line: that empties when the process drops its
// memory, which the kernel does BEFORE it closes its files, so a firecracker whose cmdline is gone
// can still have its tap open, and the next one to open it gets EBUSY. Held until the process is
// reaped, or is a zombie with no other thread left - the point at which every file is closed.
func holding(pid int, start uint64) bool {
	st, ok := procStat(pid)
	if !ok {
		return false
	}

	if start != 0 && st.start != start {
		return false // the pid was reused; ours is gone
	}

	return !((st.state == 'Z' || st.state == 'X') && st.threads <= 1)
}
