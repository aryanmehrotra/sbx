//go:build unix

package execd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// procs owns every wait on the processes execd starts.
//
// Why not exec.Cmd.Wait: as PID 1 (or a Linux child subreaper) execd inherits every orphan in
// the container, and orphans only stop being zombies if somebody calls wait4(-1). A wait4(-1)
// loop and a cmd.Wait() in another goroutine race for the same children, and whichever loses
// sees ECHILD - an exit code gone for good. So in reaping mode there is exactly one caller of
// wait4, and it hands each status to whoever started that pid. Outside reaping mode (tests, a
// developer's laptop) each process gets a wait4 on its own pid, which reaps nothing else.
type procs struct {
	reap bool

	mu      sync.Mutex
	waiting map[int]*proc

	stop chan struct{}
	once sync.Once
}

// proc is one started process. done closes once it has exited and been reaped.
type proc struct {
	pid    int
	done   chan struct{}
	status syscall.WaitStatus
	cmd    *exec.Cmd
}

func newProcs(reap bool) *procs {
	t := &procs{reap: reap, waiting: map[int]*proc{}, stop: make(chan struct{})}
	if reap {
		t.startReaper()
	}

	return t
}

// start starts cmd and returns its handle. cmd's Stdin, Stdout and Stderr must be nil or
// *os.File: any other io.Reader or io.Writer makes os/exec copy through goroutines that only
// cmd.Wait ever stops, and cmd.Wait is exactly what this type exists not to call.
func (t *procs) start(cmd *exec.Cmd) (*proc, error) {
	// The lock is held across Start and the registration so that a child which exits at once
	// cannot be reaped before it is registered: the reaper looks the pid up under this lock,
	// so it waits for the registration and then finds it.
	t.mu.Lock()
	if err := cmd.Start(); err != nil {
		t.mu.Unlock()
		return nil, err
	}

	p := &proc{pid: cmd.Process.Pid, done: make(chan struct{}), cmd: cmd}

	if t.reap {
		t.waiting[p.pid] = p
		t.mu.Unlock()

		return p, nil
	}
	t.mu.Unlock()

	go p.waitOwn()

	return p, nil
}

func (p *proc) waitOwn() {
	var ws syscall.WaitStatus

	for {
		_, err := syscall.Wait4(p.pid, &ws, 0, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}

		if err != nil {
			// ECHILD: somebody else reaped it. The status is lost; report it as a failure
			// rather than inventing a success.
			ws = syscall.WaitStatus(255 << 8)
		}

		break
	}

	p.finish(ws)
}

func (p *proc) finish(ws syscall.WaitStatus) {
	p.status = ws
	// Release drops the pidfd os.Process holds on Linux; nothing else is left to wait for.
	_ = p.cmd.Process.Release()
	close(p.done)
}

// exitCode is the shell's convention: the exit status, or 128+signal for a killed process.
func (p *proc) exitCode() int {
	return waitCode(p.status)
}

func waitCode(ws syscall.WaitStatus) int {
	switch {
	case ws.Exited():
		return ws.ExitStatus()
	case ws.Signaled():
		return 128 + int(ws.Signal())
	default:
		return 255
	}
}

// describe renders the status the way os/exec does ("exit status 3", "signal: killed"), which
// is what upstream stores as a command's error and what people already recognise.
func (p *proc) describe() string {
	switch {
	case p.status.Exited():
		return fmt.Sprintf("exit status %d", p.status.ExitStatus())
	case p.status.Signaled():
		return "signal: " + p.status.Signal().String()
	default:
		return "exited in an unknown state"
	}
}

func (t *procs) startReaper() {
	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, syscall.SIGCHLD)

	go func() {
		defer signal.Stop(sigs)

		// SIGCHLD coalesces, and a burst of exits can arrive as one signal, so every wake
		// drains until wait4 has nothing more. The ticker is the belt to that: a signal that
		// arrived between Notify and the first child costs at most a second of zombie.
		tick := time.NewTicker(time.Second)
		defer tick.Stop()

		for {
			select {
			case <-t.stop:
				return
			case <-sigs:
			case <-tick.C:
			}

			t.reapAll()
		}
	}()
}

func (t *procs) reapAll() {
	for {
		var ws syscall.WaitStatus

		pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}

		if err != nil || pid <= 0 {
			return
		}

		t.mu.Lock()
		p := t.waiting[pid]
		delete(t.waiting, pid)
		t.mu.Unlock()

		// A pid nobody registered is an orphan that was re-parented to us. Reaping it is the
		// whole point; there is nobody to tell.
		if p != nil {
			p.finish(ws)
		}
	}
}

func (t *procs) close() {
	t.once.Do(func() { close(t.stop) })
}

// signalGroup signals every process in the group led by pgid. ESRCH means the group is already
// gone, which for every caller here is success.
func signalGroup(pgid int, sig syscall.Signal) error {
	if pgid <= 0 {
		return fmt.Errorf("invalid process group %d", pgid)
	}

	err := syscall.Kill(-pgid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}

	return err
}

// groupAlive reports whether any process of the group still exists. A zombie leader counts, so
// this is only meaningful for groups whose leader somebody is reaping - which all of ours are.
func groupAlive(pgid int) bool {
	return syscall.Kill(-pgid, 0) == nil
}

// terminateGroup is upstream's interrupt: SIGTERM, up to grace for the group to go, then
// SIGKILL. It returns once the group is gone or SIGKILL has been sent.
func terminateGroup(pgid int, grace time.Duration) error {
	if err := signalGroup(pgid, syscall.SIGTERM); err != nil {
		return signalGroup(pgid, syscall.SIGKILL)
	}

	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if !groupAlive(pgid) {
			return nil
		}

		time.Sleep(20 * time.Millisecond)
	}

	return signalGroup(pgid, syscall.SIGKILL)
}
