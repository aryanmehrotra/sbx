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
// The file holds the holder's pid and start time (internal/procid), so a pid recycled to an
// unrelated process after the holder died reads as stale rather than as held.
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
	"time"

	"github.com/aryanmehrotra/sbx/internal/procid"
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

// kindWith marks a name lock as held by `sbx with` for its whole run: the sandbox under it is
// ephemeral and removed when that command ends, so a create or add is refused at once.
const kindWith = "with"

// HeldError is a lock that was held when this caller gave up on it.
type HeldError struct {
	What      string        // "the slot lock", "sandbox \"x\""
	Path      string        // the lock file, when there is one
	Holder    int           // the holding pid, 0 when it could not be read
	Waited    time.Duration // how long this caller waited
	Ephemeral bool          // held by a live `sbx with`, which removes the sandbox when it ends
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

// lock is one lock file and how to take it.
type lock struct {
	what  string
	path  string
	err   error // why there is no path; the in-process half is then all there is
	local chan struct{}
	wait  time.Duration
	kind  string // written into the file: "" or kindWith
	names bool   // a name lock, so one a live `sbx with` holds is refused at once
}

// Acquire waits for the slot lock and returns its release. onWait, if set, is called once
// with the holder's pid when the wait has gone on long enough to be worth mentioning.
//
// The release is safe to call more than once, because the caller releases it early on the
// happy path and defers it for every other path.
func Acquire(ctx context.Context, onWait func(holder int)) (func(), error) {
	path, err := Path()

	return take(ctx, lock{what: "the slot lock", path: path, err: err, local: slotLocal, wait: SlotWait}, onWait)
}

// AcquireName waits for sandbox's name lock. A name a live `sbx with` owns is not waited for:
// it returns a *HeldError with Ephemeral set at once.
func AcquireName(ctx context.Context, sandbox string, onWait func(holder int)) (func(), error) {
	return take(ctx, nameLock(sandbox, NameWait, ""), onWait)
}

// TryName takes sandbox's name lock only if nobody holds it, and otherwise returns a
// *HeldError naming the holder at once.
func TryName(sandbox string) (func(), error) {
	return take(context.Background(), nameLock(sandbox, 0, ""), nil)
}

// ClaimEphemeral is TryName for `sbx with`, which holds the name for as long as it runs. The
// lock says so, and any create or add of the name meanwhile is refused at once rather than made
// to wait for a command that may run for an hour and then removes what it made.
func ClaimEphemeral(sandbox string) (func(), error) {
	return take(context.Background(), nameLock(sandbox, 0, kindWith), nil)
}

func nameLock(sandbox string, wait time.Duration, kind string) lock {
	path, err := NamePath(sandbox)

	return lock{what: fmt.Sprintf("sandbox %q", sandbox), path: path, err: err, local: nameLocal(sandbox),
		wait: wait, kind: kind, names: true}
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
	dir, err := namesDir()
	if err != nil {
		return "", err
	}

	return filepath.Join(dir, sandbox+".lock"), nil
}

func namesDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	return filepath.Join(home, ".sbx", "locks"), nil
}

func take(ctx context.Context, l lock, onWait func(int)) (func(), error) {
	began := time.Now()
	deadline := began.Add(l.wait)
	noticed := false

	notice := func(holder int) {
		if onWait != nil && !noticed && time.Since(began) >= NoticeAfter {
			noticed = true
			onWait(holder)
		}
	}

	held := func(holder int, ephemeral bool) error {
		return &HeldError{What: l.what, Path: l.path, Holder: holder, Waited: time.Since(began), Ephemeral: ephemeral}
	}

	// A live `sbx with` owns the name for its whole run: refuse now rather than wait. Asked on
	// every pass, including while the in-process half is held, because the `with` may be in
	// this process.
	ephemeral := func() error {
		if !l.names || l.path == "" {
			return nil
		}

		if rec, kind, ok := readRecord(l.path); ok && kind == kindWith && rec.Alive() {
			return held(rec.PID, true)
		}

		return nil
	}

	// The in-process half first. Polled rather than a blocking select on a timer, so the
	// progress notice fires on the same schedule as the file half's.
	for taken := false; !taken; {
		select {
		case l.local <- struct{}{}:
			taken = true
		default:
			if err := ephemeral(); err != nil {
				return nil, err
			}

			if !time.Now().Before(deadline) {
				return nil, held(os.Getpid(), false)
			}

			notice(os.Getpid())

			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(retry):
			}
		}
	}

	unlocal := func() { <-l.local }

	if l.err != nil || l.path == "" {
		return releaseOnce(unlocal), nil
	}

	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return releaseOnce(unlocal), nil
	}

	body := procid.Self().String()
	if l.kind != "" {
		body += "\n" + l.kind
	}

	for {
		f, err := os.OpenFile(l.path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			_, _ = f.WriteString(body)
			_ = f.Close()

			return releaseOnce(func() { _ = os.Remove(l.path); unlocal() }), nil
		}

		if !errors.Is(err, os.ErrExist) {
			// Not "somebody holds it" but "this cannot be a file here" - a read-only $HOME and
			// the like. The same degradation as having no $HOME at all.
			return releaseOnce(unlocal), nil
		}

		// Held by something. If that something is gone - a create that was killed - the lock
		// is rubbish and must not block the machine for ever.
		if clearStale(l.path) {
			continue
		}

		if err := ephemeral(); err != nil {
			unlocal()

			return nil, err
		}

		rec, _, _ := readRecord(l.path)

		if !time.Now().Before(deadline) {
			unlocal()

			return nil, held(rec.PID, false)
		}

		notice(rec.PID)

		select {
		case <-ctx.Done():
			unlocal()

			return nil, ctx.Err()
		case <-time.After(retry):
		}
	}
}

// readRecord reads a lock file: the holder's record on the first line, its kind on the second.
func readRecord(path string) (procid.Record, string, bool) {
	body, err := os.ReadFile(path)
	if err != nil {
		return procid.Record{}, "", false
	}

	first, kind, _ := strings.Cut(string(body), "\n")

	rec, ok := procid.Parse(first)

	return rec, strings.TrimSpace(kind), ok
}

// stale reports whether the lock at path is rubbish: unreadable, or held by a process that is
// no longer running - including one whose pid now belongs to something else.
func stale(path string) bool {
	body, err := os.ReadFile(path)
	if err != nil {
		return false
	}

	rec, _, ok := readRecord(path)
	if !ok {
		// Empty is also what a lock looks like in the instant between its creator's open and
		// its write, and removing it then would let two holders in. So an empty file is only
		// rubbish once it has stayed empty for a while.
		if len(body) == 0 {
			if fi, err := os.Stat(path); err != nil || time.Since(fi.ModTime()) < time.Second {
				return false
			}
		}

		return true
	}

	if rec.PID == os.Getpid() {
		return false // ours, held by another goroutine; do not delete it underneath it
	}

	return !rec.Alive()
}

// clearStale removes a lock whose owner is no longer running, and reports whether it did.
func clearStale(path string) bool {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return true // gone already: try again at once
	}

	if !stale(path) {
		return false
	}

	_ = os.Remove(path)

	return true
}

// Stale lists the lock files - the slot lock and every name lock - whose holder is gone. A
// stale lock is cleared by the next caller for the same name, which may never come; `sbx gc`
// asks this instead.
func Stale() ([]string, error) {
	var paths []string

	if p, err := Path(); err == nil {
		paths = append(paths, p)
	}

	dir, err := namesDir()
	if err != nil {
		return nil, err
	}

	names, err := filepath.Glob(filepath.Join(dir, "*.lock"))
	if err != nil {
		return nil, err
	}

	var out []string

	for _, p := range append(paths, names...) {
		if _, err := os.Stat(p); err != nil {
			continue
		}

		if stale(p) {
			out = append(out, p)
		}
	}

	return out, nil
}

// RemoveStale removes the locks Stale listed, each re-checked first: a lock taken again since
// the listing belongs to a live holder now.
func RemoveStale(paths []string) error {
	var errs []error

	for _, p := range paths {
		if !stale(p) {
			continue
		}

		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// releaseOnce wraps a cleanup so it runs exactly once, however many times the caller calls it.
// Create releases early on the happy path and defers it for every other path, so more than
// once is the normal case rather than a mistake.
func releaseOnce(cleanup func()) func() {
	var once sync.Once

	return func() { once.Do(cleanup) }
}

// EphemeralHolder reports the pid of the live `sbx with` that owns sandbox's name, if one does.
// `sbx rm` asks it: the sandbox under that lock is removed when the `with` ends, and taking it
// away earlier pulls it out from under a command that is still using it.
func EphemeralHolder(sandbox string) (int, bool) {
	path, err := NamePath(sandbox)
	if err != nil {
		return 0, false
	}

	if rec, kind, ok := readRecord(path); ok && kind == kindWith && rec.Alive() {
		return rec.PID, true
	}

	return 0, false
}

// ClearStaleName removes sandbox's name lock when its holder is no longer running, and reports
// the path it removed. A live holder's lock is never touched. `sbx rm` calls it: a create killed
// part-way leaves both its sandbox and its lock, and removing one left the other for `sbx gc`.
func ClearStaleName(sandbox string) (string, bool) {
	path, err := NamePath(sandbox)
	if err != nil {
		return "", false
	}

	if _, err := os.Stat(path); err != nil || !stale(path) {
		return "", false
	}

	if err := os.Remove(path); err != nil {
		return "", false
	}

	return path, true
}
