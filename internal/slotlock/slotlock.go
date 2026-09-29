package slotlock

// Claiming a port slot, or a sandbox name, without racing another sbx on the same machine.
//
// AllocSlot reads the sandboxes that exist and returns the first free slot. Two creates
// running at the same moment read the same state and pick the same slot, and the ports only
// actually get claimed when a container is created - so the loser fails at `docker run` with
// "failed to set up container networking: driver failed programming external connectivity".
// Measured, four concurrent creates on one machine: three of them failed.
//
// That is not a rare shape. docs/GUIDES.md describes a sandbox per CI job on a persistent
// runner, which is several creates arriving together by construction.
//
// The slot lock is held from AllocSlot until the FIRST container exists, not for the whole
// create. Once a container carries the slot label and holds the ports, every other AllocSlot
// can see it, and the rest of the work - health checks, init, the other services - happens
// unserialised.
//
// A wait that runs out is an error, never a licence to go ahead. It used to return without the
// lock after 90 seconds, on the theory that racing beat waiting; but the holder then was a
// create that kept the lock through its health wait, so a slow service made every waiter give
// up at once and they all took the same slot. Measured: four creates, two sandboxes listed at
// one address, one `docker run` failing with "port is already allocated". Racing is exactly
// the failure this exists to prevent, so the caller is told who holds it and what to do.
//
// The name lock is the same thing keyed by sandbox name. `sbx with X` checks X is unused and
// then creates it, and `sbx create X` checks what exists and then fills in the rest; without a
// lock between the check and the create, two of them interleave. Measured: two `sbx with qa-cc`
// started together, one reused the other's containers, and its teardown removed the sandbox
// while the other run was still using it.
//
// Both are files under $HOME, so they cover one machine and not a shared remote DOCKER_HOST.
// A pid file rather than flock(2): it is the mechanism this lock already had, it builds on all
// eight platforms without build tags, and the holder's pid is what the error needs to print.
// A machine with no usable $HOME gets the in-process half only, as before: refusing every
// create there would wedge a working command over a race it may never have.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	// SlotWait bounds the wait for the slot lock. It is held only until a create's first
	// container exists, which is one `docker run` - but that includes pulling the image, and a
	// large image on a slow link takes minutes. Variables so tests can shorten them.
	SlotWait = 10 * time.Minute

	// NameWait bounds the wait for a sandbox's name lock. A create holds it through every
	// service's health wait, so it is as long as the slot wait and for the same reason.
	NameWait = 10 * time.Minute

	// NoticeAfter is how long a waiter stays quiet before saying what it is waiting for.
	NoticeAfter = 2 * time.Second
)

const retry = 50 * time.Millisecond

// HeldError is a lock that was still held when the wait ran out.
type HeldError struct {
	What   string        // "the slot lock", "sandbox \"x\""
	Path   string        // the lock file, when there is one
	Holder int           // the holding pid, 0 when it could not be read
	Waited time.Duration // how long this caller waited
}

func (e *HeldError) Error() string {
	holder := "another sbx"
	if e.Holder > 0 {
		holder = "pid " + strconv.Itoa(e.Holder)
	}

	msg := fmt.Sprintf("%s is held by %s", e.What, holder)

	// TryName does not wait, and "after 0s" reads like a timeout that fired too early.
	if e.Waited >= time.Second {
		msg = fmt.Sprintf("%s is still held by %s after %s, so this did not go ahead without it",
			e.What, holder, e.Waited.Round(time.Second))
	}

	if e.Holder > 0 && e.Holder != os.Getpid() {
		msg += fmt.Sprintf("\n     see what it is doing: ps -p %d -o pid,etime,command", e.Holder)

		if e.Path != "" {
			msg += fmt.Sprintf("\n     if that is not an sbx, remove the lock and re-run: rm %s", e.Path)
		}
	}

	return msg
}

// Within one process too, not only between them. The file lock records a pid, and a pid
// cannot distinguish two goroutines - so concurrent callers here would each see their own
// pid in the file, conclude the holder is alive, and spin. A channel rather than a mutex so a
// waiter can give up on a deadline or a cancelled context.
var (
	slotLocal = make(chan struct{}, 1)

	namesMu    sync.Mutex
	nameLocals = map[string]chan struct{}{}
)

func nameLocal(sandbox string) chan struct{} {
	namesMu.Lock()
	defer namesMu.Unlock()

	c, ok := nameLocals[sandbox]
	if !ok {
		c = make(chan struct{}, 1)
		nameLocals[sandbox] = c
	}

	return c
}

// Acquire waits for the slot lock and returns its release. onWait, if set, is called once
// with the holder's pid when the wait has gone on long enough to be worth mentioning.
//
// The release is safe to call more than once, because the caller releases it early on the
// happy path and defers it for every other path.
func Acquire(ctx context.Context, onWait func(holder int)) (func(), error) {
	path, err := Path()

	return take(ctx, "the slot lock", path, err, slotLocal, SlotWait, onWait)
}

// AcquireName waits for sandbox's name lock.
func AcquireName(ctx context.Context, sandbox string, onWait func(holder int)) (func(), error) {
	path, err := NamePath(sandbox)

	return take(ctx, fmt.Sprintf("sandbox %q", sandbox), path, err, nameLocal(sandbox), NameWait, onWait)
}

// TryName takes sandbox's name lock only if nobody holds it, and otherwise returns a
// *HeldError naming the holder at once.
func TryName(sandbox string) (func(), error) {
	path, err := NamePath(sandbox)

	return take(context.Background(), fmt.Sprintf("sandbox %q", sandbox), path, err, nameLocal(sandbox), 0, nil)
}

func Path() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	return filepath.Join(home, ".sbx", "slots.lock"), nil
}

// NamePath is where sandbox's name lock lives. Names are validated before they get here, so
// they are safe as a file name.
func NamePath(sandbox string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	return filepath.Join(home, ".sbx", "locks", sandbox+".lock"), nil
}

func take(ctx context.Context, what, path string, pathErr error, local chan struct{}, wait time.Duration,
	onWait func(int),
) (func(), error) {
	began := time.Now()
	deadline := began.Add(wait)
	noticed := false

	notice := func(holder int) {
		if onWait != nil && !noticed && time.Since(began) >= NoticeAfter {
			noticed = true
			onWait(holder)
		}
	}

	held := func(holder int) error {
		return &HeldError{What: what, Path: path, Holder: holder, Waited: time.Since(began)}
	}

	// The in-process half first. Polled rather than a blocking select on a timer, so the
	// progress notice fires on the same schedule as the file half's.
	for taken := false; !taken; {
		select {
		case local <- struct{}{}:
			taken = true
		default:
			if !time.Now().Before(deadline) {
				return nil, held(os.Getpid())
			}

			notice(os.Getpid())

			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(retry):
			}
		}
	}

	unlocal := func() { <-local }

	if pathErr != nil || path == "" {
		return releaseOnce(unlocal), nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return releaseOnce(unlocal), nil
	}

	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			_, _ = fmt.Fprintf(f, "%d", os.Getpid())
			_ = f.Close()

			return releaseOnce(func() { _ = os.Remove(path); unlocal() }), nil
		}

		if !errors.Is(err, os.ErrExist) {
			// Not "somebody holds it" but "this cannot be a file here" - a read-only $HOME and
			// the like. The same degradation as having no $HOME at all.
			return releaseOnce(unlocal), nil
		}

		// Held by something. If that something is gone - a create that was killed - the lock
		// is rubbish and must not block the machine for ever.
		if clearStale(path) {
			continue
		}

		holder := readHolder(path)

		if !time.Now().Before(deadline) {
			unlocal()

			return nil, held(holder)
		}

		notice(holder)

		select {
		case <-ctx.Done():
			unlocal()

			return nil, ctx.Err()
		case <-time.After(retry):
		}
	}
}

func readHolder(path string) int {
	body, err := os.ReadFile(path)
	if err != nil {
		return 0
	}

	pid, _ := strconv.Atoi(strings.TrimSpace(string(body)))

	return pid
}

// clearStale removes a lock whose owner is no longer running, and reports whether it did.
func clearStale(path string) bool {
	body, err := os.ReadFile(path)
	if err != nil {
		return errors.Is(err, os.ErrNotExist) // gone already: try again at once
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(body)))
	if err != nil || pid <= 0 {
		// Empty is also what a lock looks like in the instant between its creator's open and
		// its write, and removing it then would let two holders in. So an empty file is only
		// rubbish once it has stayed empty for a while.
		if len(body) == 0 {
			if fi, err := os.Stat(path); err != nil || time.Since(fi.ModTime()) < time.Second {
				return false
			}
		}

		_ = os.Remove(path)

		return true
	}

	if pid == os.Getpid() {
		return false // ours, held by another goroutine; do not delete it underneath it
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		_ = os.Remove(path)

		return true
	}

	// EPERM is a process that exists and belongs to someone else - alive, so still the holder.
	if err := proc.Signal(syscall.Signal(0)); err != nil && !errors.Is(err, syscall.EPERM) {
		_ = os.Remove(path)

		return true
	}

	return false
}

// releaseOnce wraps a cleanup so it runs exactly once, however many times the caller calls it.
// Create releases early on the happy path and defers it for every other path, so more than
// once is the normal case rather than a mistake.
func releaseOnce(cleanup func()) func() {
	var once sync.Once

	return func() { once.Do(cleanup) }
}
