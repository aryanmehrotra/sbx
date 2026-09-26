package fc

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// slowEngine is an image that takes a while to export, and counts how often it is asked to.
type slowEngine struct {
	tar            []byte
	pulls, exports atomic.Int32
	present        atomic.Bool
}

func (e *slowEngine) Inspect(context.Context, string) (ImageConfig, error) {
	if !e.present.Load() {
		return ImageConfig{}, ErrNoImage
	}

	return ImageConfig{ID: testID, Cmd: []string{"python"}}, nil
}

func (e *slowEngine) Pull(context.Context, string) error {
	e.pulls.Add(1)
	time.Sleep(50 * time.Millisecond)
	e.present.Store(true)

	return nil
}

func (e *slowEngine) Export(_ context.Context, _ string, w io.Writer) error {
	e.exports.Add(1)
	time.Sleep(100 * time.Millisecond)
	_, err := w.Write(e.tar)

	return err
}

func (e *slowEngine) CopyOut(context.Context, string, string, string) error { return nil }

type lockedExt4 struct {
	mu sync.Mutex
	fakeExt4
}

func (l *lockedExt4) Build(ctx context.Context, src, img, label string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.fakeExt4.Build(ctx, src, img, label)
}

// Four creates of one ~2.5 GB image at once each pulled, exported and mkfs'd it themselves, so
// the first create on a fresh host took four builds' worth of disk and CPU and every one of them
// missed the SDK's wait. Concurrent builds of one image now share one build.
func TestConcurrentBuildsOfOneImageShareOneBuild(t *testing.T) {
	eng := &slowEngine{tar: imageTar(t)}
	b := &RootfsBuilder{Dir: t.TempDir(), Engine: eng, Ext4: &lockedExt4{fakeExt4: fakeExt4{tar: true}}, Headroom: 1 << 20}

	var wg sync.WaitGroup

	paths := make([]string, 6)
	errs := make([]error, 6)

	for i := range paths {
		wg.Go(func() {
			r, err := b.Build(context.Background(), "opensandbox/code-interpreter:v1")
			paths[i], errs[i] = r.Path, err
		})
	}

	wg.Wait()

	for i := range paths {
		if errs[i] != nil || paths[i] != paths[0] {
			t.Fatalf("build %d: %q, %v (want %q)", i, paths[i], errs[i], paths[0])
		}
	}

	if p, x := eng.pulls.Load(), eng.exports.Load(); p != 1 || x != 1 {
		t.Fatalf("six concurrent builds pulled %d times and exported %d times, want 1 and 1", p, x)
	}
}

// A waiter whose own request ends stops waiting; the build it was waiting on is not cut short for
// the others.
func TestABuildWaiterThatGivesUpDoesNotCancelTheBuild(t *testing.T) {
	eng := &slowEngine{tar: imageTar(t)}
	b := &RootfsBuilder{Dir: t.TempDir(), Engine: eng, Ext4: &lockedExt4{fakeExt4: fakeExt4{tar: true}}, Headroom: 1 << 20}

	first, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { _, err := b.Build(first, "img"); done <- err }()

	time.Sleep(10 * time.Millisecond)

	second := make(chan error, 1)
	go func() { _, err := b.Build(context.Background(), "img"); second <- err }()

	time.Sleep(10 * time.Millisecond)
	cancel()

	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("the cancelled caller got %v", err)
	}

	if err := <-second; err != nil {
		t.Fatalf("the other caller's build failed with the first's cancel: %v", err)
	}
}
