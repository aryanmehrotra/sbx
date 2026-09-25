package osb

// What an image declares - entrypoint, OS, architecture - asked once per burst, not once per
// create.
//
// Each create inspected its image with the docker CLI: a process per create. A hundred at once
// took about a second to get through that alone (measured, SBX_OSB_TRACE, cold burst of 100 on
// colima), to learn the same answer a hundred times. Answers are kept for imageTTL and
// concurrent askers for one image share a single inspect. Short, because a tag can be re-pulled
// to point somewhere else, and a create a few seconds after that should see it.

import (
	"context"
	"sync"
	"time"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

const imageTTL = 10 * time.Second

type imageEntry struct {
	done chan struct{}
	info provider.ImageInfo
	err  error
	at   time.Time
}

type imageCache struct {
	mu      sync.Mutex
	entries map[string]*imageEntry
}

// info returns inspect(image), shared with any caller asking for the same image at the same
// time and reused for imageTTL. Failures are not kept: a missing image is about to be pulled.
func (c *imageCache) info(ctx context.Context, image string, inspect func(context.Context, string) (provider.ImageInfo, error)) (provider.ImageInfo, error) {
	c.mu.Lock()

	if c.entries == nil {
		c.entries = map[string]*imageEntry{}
	}

	e, ok := c.entries[image]
	if ok {
		select {
		case <-e.done:
			if e.err != nil || time.Since(e.at) > imageTTL {
				ok = false
			}
		default: // in flight: share it
		}
	}

	if !ok {
		e = &imageEntry{done: make(chan struct{})}
		c.entries[image] = e
		c.mu.Unlock()

		e.info, e.err = inspect(context.WithoutCancel(ctx), image)
		e.at = time.Now()
		close(e.done)

		return e.info, e.err
	}
	c.mu.Unlock()

	select {
	case <-e.done:
		return e.info, e.err
	case <-ctx.Done():
		return provider.ImageInfo{}, ctx.Err()
	}
}

// forget drops an image, after a pull has changed what it is.
func (c *imageCache) forget(image string) {
	c.mu.Lock()
	delete(c.entries, image)
	c.mu.Unlock()
}
