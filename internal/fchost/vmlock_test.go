package fchost

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// slowStart is a runner whose `limactl start` takes a while and counts how many run at once.
type slowStart struct {
	*fakeRunner
	inFlight, most atomic.Int32
}

func (s *slowStart) Run(ctx context.Context, c Cmd) error {
	if strings.Join(c.Argv, " ") == "limactl start --tty=false sbx-fc" {
		n := s.inFlight.Add(1)
		for {
			m := s.most.Load()
			if n <= m || s.most.CompareAndSwap(m, n) {
				break
			}
		}

		time.Sleep(100 * time.Millisecond)
		s.inFlight.Add(-1)
	}

	return s.fakeRunner.Run(ctx, c)
}

// Two commands that both find the helper VM stopped must not both start it: colima's start
// switches the global docker context, and two overlapping guards can restore the wrong one.
func TestConcurrentEnsuresStartTheVMOneAtATime(t *testing.T) {
	r := &slowStart{fakeRunner: (&fakeRunner{}).on("limactl list", `{"name":"sbx-fc","status":"Stopped"}`, nil)}
	m := &Manager{Driver: lima{}, Config: testCfg, Run: r, Out: io.Discard, StateDir: t.TempDir()}

	var wg sync.WaitGroup

	for range 3 {
		wg.Add(1)

		go func() {
			defer wg.Done()
			_ = m.Ensure(context.Background(), EnsureOptions{Binary: func(context.Context) (string, error) { return "", nil }})
		}()
	}

	wg.Wait()

	if most := r.most.Load(); most != 1 {
		t.Fatalf("%d helper-VM starts ran at once", most)
	}

	if _, err := os.Stat(m.lockPath()); !os.IsNotExist(err) {
		t.Fatalf("the lock file outlived the Ensures: %v", err)
	}
}

// A lock left by a process that no longer exists does not block the next command; one held by a
// live process does, until the wait runs out, and says who holds it.
func TestAStaleHelperVMLockIsCleared(t *testing.T) {
	m := &Manager{Config: testCfg, Out: io.Discard, StateDir: t.TempDir()}

	if err := os.WriteFile(m.lockPath(), []byte("999999999"), 0o600); err != nil {
		t.Fatal(err)
	}

	unlock, err := m.lockVM(context.Background())
	if err != nil {
		t.Fatalf("a dead owner's lock blocked: %v", err)
	}

	unlock()

	saved := vmLockWait
	vmLockWait = 100 * time.Millisecond
	t.Cleanup(func() { vmLockWait = saved })

	live := strconv.Itoa(os.Getppid()) // alive, and not us
	if err := os.WriteFile(filepath.Join(m.StateDir, "vm-"+testCfg.Name+".lock"), []byte(live), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := m.lockVM(context.Background()); err == nil || !strings.Contains(err.Error(), "pid "+live) {
		t.Fatalf("a live owner's lock = %v", err)
	}
}
