package fchost

// One helper-VM lifecycle change at a time on this machine.
//
// Two commands that both find the VM stopped both start it, and colima's start switches the
// global docker context: each guard reads the context before the other's start and puts back
// whichever it saw, so the loser can "restore" colima's own context over the person's. Two
// Ensures also race the binary install and the daemon restart. So Ensure, Stop and Remove hold a
// lock file under the state directory for their whole run.
//
// The same shape as internal/slotlock: an O_EXCL file holding the owner's pid and start time
// (internal/procid), cleared when that process is gone - or when its pid now belongs to another
// process, which a bare pid could not tell and which blocked every Ensure for the whole wait - plus
// an in-process mutex because a pid cannot tell two goroutines apart. A lock written before the
// start time was kept (a bare pid) is still read, and judged by its pid.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/aryanmehrotra/sbx/internal/procid"
)

// vmLockWait is how long a command waits for another one's VM start: a first create downloads
// an image and provisions docker, which is minutes, not seconds.
var vmLockWait = 15 * time.Minute

var vmLockLocal sync.Mutex

func (m *Manager) lockPath() string { return filepath.Join(m.StateDir, "vm-"+m.Config.Name+".lock") }

// lockVM blocks until this process holds the helper VM's lifecycle lock, and returns its release.
func (m *Manager) lockVM(ctx context.Context) (func(), error) {
	vmLockLocal.Lock()

	if err := os.MkdirAll(m.StateDir, 0o700); err != nil {
		vmLockLocal.Unlock()
		return nil, err
	}

	path := m.lockPath()
	deadline := time.Now().Add(vmLockWait)
	said := false

	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = f.WriteString(procid.Self().String())
			_ = f.Close()

			var once sync.Once

			return func() {
				once.Do(func() {
					_ = os.Remove(path)
					vmLockLocal.Unlock()
				})
			}, nil
		}

		if !errors.Is(err, os.ErrExist) {
			vmLockLocal.Unlock()
			return nil, err
		}

		holder := clearStaleVMLock(path)
		if holder == 0 {
			continue
		}

		who := "another sbx"
		if holder > 0 {
			who = fmt.Sprintf("another sbx (pid %d)", holder)
		}

		if !said {
			m.say("waiting for %s that is starting or changing %s", who, m.Config.Name)
			said = true
		}

		if time.Now().After(deadline) {
			vmLockLocal.Unlock()
			return nil, fmt.Errorf("%s has held the helper VM lock %s for %s; if it "+
				"is stuck, stop it and run the command again", who, path, vmLockWait)
		}

		select {
		case <-ctx.Done():
			vmLockLocal.Unlock()
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// clearStaleVMLock removes a lock whose owner is gone and returns 0, or returns the live owner.
func clearStaleVMLock(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0 // released meanwhile
	}

	rec, ok := procid.Parse(string(b))
	if !ok {
		// Written by nobody who finished writing it. Give a writer a moment before calling it
		// rubbish: the pid lands a microsecond after the create.
		if fi, serr := os.Stat(path); serr == nil && time.Since(fi.ModTime()) < time.Second {
			return -1
		}

		_ = os.Remove(path)

		return 0
	}

	if rec.PID != os.Getpid() && !rec.Alive() {
		_ = os.Remove(path)
		return 0
	}

	return rec.PID
}
