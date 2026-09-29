package cli

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"
)

// The lock has one job: two callers must not be inside it at once.
func TestSlotLockIsExclusive(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	var (
		mu     sync.Mutex
		inside int
		worst  int
		wg     sync.WaitGroup
	)

	for range 8 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			release, err := lockSlots(context.Background())
			if err != nil {
				t.Error(err)
				return
			}
			defer release()

			mu.Lock()
			inside++

			if inside > worst {
				worst = inside
			}
			mu.Unlock()

			time.Sleep(5 * time.Millisecond)

			mu.Lock()
			inside--
			mu.Unlock()
		}()
	}

	wg.Wait()

	if worst > 1 {
		t.Errorf("%d callers were inside the lock at once - it is not exclusive", worst)
	}
}

// Releasing twice is normal here: Create releases early on the happy path and defers it for
// every other path. The second call must not delete a lock somebody else now holds.
func TestReleasingTwiceIsSafe(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	release, err := lockSlots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	release()
	release()

	// Still takeable, and taking it once more must not be blocked by the double release.
	second, err := lockSlots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer second()

	path, err := slotLockPath()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := readFileExists(path); err != nil {
		t.Errorf("the lock is not held after being taken again: %v", err)
	}
}

func readFileExists(path string) (bool, error) {
	if _, err := os.Stat(path); err != nil {
		return false, err
	}

	return true, nil
}
