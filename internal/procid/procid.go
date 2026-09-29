// Package procid identifies a process across pid reuse.
//
// sbx records "who holds this" in files - the slot and sandbox-name locks, the daemon registry,
// snapshot's pause marks - and decides a record is stale when its process is gone. A pid alone
// cannot say that: once the holder dies its pid is free, and when the system hands it to an
// unrelated process the record reads as held for as long as that process lives. A lock named
// by a recycled pid blocked its sandbox name (`sbx with` refused it, `sbx create` waited ten
// minutes) with nobody holding it.
//
// So a record carries the process's start time as well, and is the holder only while both
// match. The start time comes from the kernel without running anything: /proc/<pid>/stat on
// Linux, the kern.proc.pid sysctl on macOS. Where neither exists (the BSDs, Windows) StartOf
// says it cannot tell, the record carries the pid alone, and liveness is the pid's, as before.
package procid

import (
	"strconv"
	"strings"
	"sync"
)

// Record is a process: its pid and, where the platform can tell, its start time. Start is
// opaque - clock ticks since boot on Linux, microseconds since the epoch on macOS - and only
// ever compared with another reading on the same machine. 0 means unknown.
type Record struct {
	PID   int
	Start int64
}

var (
	selfOnce sync.Once
	self     Record
)

// Self is this process's record.
func Self() Record {
	selfOnce.Do(func() {
		self = Record{PID: pid()}
		if s, ok := StartOf(self.PID); ok {
			self.Start = s
		}
	})

	return self
}

// Alive reports whether r's process is still running - the same process, not merely one with
// its pid.
func (r Record) Alive() bool {
	if r.PID <= 0 {
		return false
	}

	if r.PID == pid() {
		return r.Start == 0 || r.Start == Self().Start
	}

	if !exists(r.PID) {
		return false
	}

	if r.Start == 0 {
		return true // written without a start time: all there is to go on is the pid
	}

	start, ok := StartOf(r.PID)
	if !ok {
		return true // a platform that cannot tell; the pid exists, so it is believed
	}

	return start == r.Start
}

// String is the record as it is written to a file: "pid" or "pid start".
func (r Record) String() string {
	if r.Start == 0 {
		return strconv.Itoa(r.PID)
	}

	return strconv.Itoa(r.PID) + " " + strconv.FormatInt(r.Start, 10)
}

// Parse reads what String wrote, and a bare pid - the format every record had before start
// times were kept.
func Parse(s string) (Record, bool) {
	fields := strings.Fields(s)
	if len(fields) == 0 || len(fields) > 2 {
		return Record{}, false
	}

	pid, err := strconv.Atoi(fields[0])
	if err != nil || pid <= 0 {
		return Record{}, false
	}

	r := Record{PID: pid}

	if len(fields) == 2 {
		if r.Start, err = strconv.ParseInt(fields[1], 10, 64); err != nil || r.Start <= 0 {
			return Record{}, false
		}
	}

	return r, true
}
