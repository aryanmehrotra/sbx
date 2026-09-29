package slotlock

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// holdAs writes a lock file owned by pid, as another live sbx process would leave it.
func holdAs(t *testing.T, path string, pid int) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(strconv.Itoa(pid)), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A wait that runs out used to return a release function as if the lock were held, and the
// caller went on to allocate a slot unlocked. Four creates behind one slow health check all
// timed out together and took the same slot. Running out is an error that names the holder.
func TestSlotWaitThatRunsOutIsAnErrorNotAnUnlockedRun(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	defer func(w time.Duration) { SlotWait = w }(SlotWait)
	SlotWait = 200 * time.Millisecond

	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}

	holder := os.Getppid() // alive, and not us
	holdAs(t, path, holder)

	release, err := Acquire(context.Background(), nil)
	if err == nil {
		release()
		t.Fatal("the wait ran out and Acquire went ahead without the lock")
	}

	var he *HeldError
	if !errors.As(err, &he) || he.Holder != holder {
		t.Fatalf("want a HeldError naming pid %d, got %v", holder, err)
	}

	for _, want := range []string{strconv.Itoa(holder), "rm " + path} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not say %q: %v", want, err)
		}
	}

	if body, _ := os.ReadFile(path); string(body) != strconv.Itoa(holder) {
		t.Errorf("a live holder's lock was replaced: %q", body)
	}
}

// Two callers for the same sandbox name are never inside at once; different names do not
// wait on each other.
func TestNameLockIsExclusivePerName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	var (
		mu            sync.Mutex
		inside, worst int
		wg            sync.WaitGroup
	)

	for range 8 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			release, err := AcquireName(context.Background(), "same", nil)
			if err != nil {
				t.Error(err)
				return
			}
			defer release()

			mu.Lock()
			inside++
			worst = max(worst, inside)
			mu.Unlock()

			time.Sleep(5 * time.Millisecond)

			mu.Lock()
			inside--
			mu.Unlock()
		}()
	}

	wg.Wait()

	if worst > 1 {
		t.Errorf("%d callers were inside one name's lock at once", worst)
	}

	a, err := AcquireName(context.Background(), "a", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer a()

	b, err := TryName("b")
	if err != nil {
		t.Fatalf("holding a's lock blocked b's: %v", err)
	}
	b()
}

// TryName refuses at once, naming the holder - in this process or another.
func TestTryNameRefusesAHeldName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	release, err := AcquireName(context.Background(), "x", nil)
	if err != nil {
		t.Fatal(err)
	}

	began := time.Now()

	if r, err := TryName("x"); err == nil {
		r()
		t.Fatal("TryName took a name another caller holds")
	}

	if time.Since(began) > time.Second {
		t.Errorf("TryName waited %s; it must not wait", time.Since(began))
	}

	release()

	r, err := TryName("x")
	if err != nil {
		t.Fatalf("a released name is still refused: %v", err)
	}
	r()

	path, _ := NamePath("y")
	holdAs(t, path, os.Getppid())

	var he *HeldError
	if _, err := TryName("y"); !errors.As(err, &he) || he.Holder != os.Getppid() {
		t.Fatalf("want a HeldError naming the other process, got %v", err)
	}
}

// A lock left by a process that is gone does not block anyone.
func TestAStaleLockIsCleared(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	path, _ := NamePath("z")
	holdAs(t, path, 1<<22+12345) // no such process

	r, err := TryName("z")
	if err != nil {
		t.Fatalf("a dead holder's lock blocked the name: %v", err)
	}
	r()
}

// A waiter says what it is waiting for, once, instead of hanging silently for minutes.
func TestAWaiterIsToldWhoHoldsTheLock(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	defer func(n time.Duration) { NoticeAfter = n }(NoticeAfter)
	NoticeAfter = 50 * time.Millisecond

	path, _ := Path()
	holdAs(t, path, os.Getppid())

	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = os.Remove(path)
	}()

	var told []int

	release, err := Acquire(context.Background(), func(pid int) { told = append(told, pid) })
	if err != nil {
		t.Fatal(err)
	}
	release()

	if len(told) != 1 || told[0] != os.Getppid() {
		t.Errorf("want one notice naming pid %d, got %v", os.Getppid(), told)
	}
}

// Releasing twice is normal (early on the happy path, deferred for every other) and must not
// delete a lock someone else took in between.
func TestReleasingTwiceDoesNotFreeTheNextHolder(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	first, err := Acquire(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	first()

	second, err := Acquire(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer second()

	first()

	path, _ := Path()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("a second release of the first holder removed the second's lock: %v", err)
	}
}
