package osb

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

func TestImageCacheSharesAnInspectAndDoesNotKeepFailures(t *testing.T) {
	var c imageCache

	var calls atomic.Int32

	fail := atomic.Bool{}

	inspect := func(context.Context, string) (provider.ImageInfo, error) {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)

		if fail.Load() {
			return provider.ImageInfo{}, errors.New("no such image")
		}

		return provider.ImageInfo{Arch: "arm64"}, nil
	}

	var wg sync.WaitGroup

	for range 30 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			if info, err := c.info(context.Background(), "node:22-slim", inspect); err != nil || info.Arch != "arm64" {
				t.Errorf("info = %+v, %v", info, err)
			}
		}()
	}

	wg.Wait()

	if n := calls.Load(); n != 1 {
		t.Fatalf("%d inspects for 30 concurrent askers, want 1", n)
	}

	c.forget("node:22-slim")
	fail.Store(true)

	if _, err := c.info(context.Background(), "node:22-slim", inspect); err == nil {
		t.Fatal("a failing inspect reported success")
	}

	fail.Store(false)

	if _, err := c.info(context.Background(), "node:22-slim", inspect); err != nil {
		t.Fatalf("a failure was cached: %v", err)
	}
}
