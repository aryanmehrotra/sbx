package fc

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestParseProcStat(t *testing.T) {
	// comm may contain spaces and ')': fields are counted from the LAST ')'.
	line := "4242 (fire cracker) x) Z 1 4242 4242 0 -1 4194560 0 0 0 0 0 0 0 0 20 0 1 0 987654 0 0"

	st, ok := parseProcStat(line)
	if !ok || st.state != 'Z' || st.threads != 1 || st.start != 987654 {
		t.Fatalf("parsed %+v, %v", st, ok)
	}
}

// A process holds its files until it is reaped, or is a zombie with no thread left - not merely
// until its command line empties. And a reused pid (another start time) is not the one we killed.
func TestHoldingFollowsTheProcessNotItsCommandLine(t *testing.T) {
	self := os.Getpid()
	start := procStart(self)

	if start == 0 || !holding(self, start) || holding(self, start+1) {
		t.Fatalf("self: start %d, holding %v, other start %v", start, holding(self, start), holding(self, start+1))
	}

	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skip(err)
	}

	pid := cmd.Process.Pid
	cs := procStart(pid)

	_ = cmd.Process.Kill()

	// Unreaped, it is a single-threaded zombie: every file closed, so no longer holding.
	deadline := time.Now().Add(5 * time.Second)
	for holding(pid, cs) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	if holding(pid, cs) {
		t.Fatal("a killed, exited process still reads as holding")
	}

	_ = cmd.Wait()

	if holding(pid, cs) {
		t.Fatal("a reaped process reads as holding")
	}
}
