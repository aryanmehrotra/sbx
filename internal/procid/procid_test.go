package procid

import (
	"os"
	"testing"
)

func TestThisProcessIsAlive(t *testing.T) {
	if !Self().Alive() {
		t.Fatal("this process's own record reads as dead")
	}
}

// The failure this package exists for: a record whose pid now belongs to a different process -
// the holder died and the pid was handed out again - must read as dead, or a lock it names is
// held by nothing for ever. Modelled with a live pid (the test's parent) and a start time that
// is not its own.
func TestALivePidWithAnotherStartTimeIsNotTheHolder(t *testing.T) {
	parent := os.Getppid()

	start, ok := StartOf(parent)
	if !ok {
		t.Skip("this platform cannot tell a process's start time; records are pid-only here")
	}

	if !(Record{PID: parent, Start: start}).Alive() {
		t.Fatal("the parent's own record reads as dead")
	}

	if (Record{PID: parent, Start: start + 1}).Alive() {
		t.Fatal("a recycled pid (same pid, different start) reads as the original holder")
	}
}

// A record written before start times were kept names only a pid, and is judged as before.
func TestAPidOnlyRecordIsJudgedByThePid(t *testing.T) {
	if !(Record{PID: os.Getppid()}).Alive() {
		t.Error("a pid-only record of a live process reads as dead")
	}

	if (Record{PID: 1<<22 + 12345}).Alive() {
		t.Error("a pid-only record of no process reads as alive")
	}
}

func TestRecordsRoundTrip(t *testing.T) {
	for _, r := range []Record{{PID: 42}, {PID: 42, Start: 1727600000123456}, Self()} {
		got, ok := Parse(r.String())
		if !ok || got != r {
			t.Errorf("Parse(%q) = %+v, %v; want %+v", r.String(), got, ok, r)
		}
	}

	if got, ok := Parse(" 77\n"); !ok || got != (Record{PID: 77}) {
		t.Errorf("a legacy pid-only file did not parse: %+v %v", got, ok)
	}

	for _, bad := range []string{"", "x", "-3", "0", "12 y"} {
		if _, ok := Parse(bad); ok {
			t.Errorf("Parse(%q) accepted garbage", bad)
		}
	}
}
