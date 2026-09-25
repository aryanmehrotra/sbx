//go:build unix

package execd

import "sync"

// replaySize is how much of a PTY session's output is kept for reconnects and viewers:
// upstream's 1 MiB, so a client that reconnects with since= sees the same scrollback either way.
const replaySize = 1 << 20

// replayBuffer is a ring of the last size bytes written, addressed by absolute offset: the byte
// at offset o (o >= oldest retained) is buf[o % size]. Offsets never reset, so a client can say
// "from where I was" across reconnects and learn from the returned offset whether it lost bytes
// to eviction in between.
type replayBuffer struct {
	mu    sync.Mutex
	buf   []byte
	total int64

	// changed is closed by the next write and replaced lazily, so every subscriber waiting on
	// one generation wakes together, and a session nobody watches allocates nothing per chunk.
	changed chan struct{}
}

func newReplayBuffer(size int) *replayBuffer {
	return &replayBuffer{buf: make([]byte, size)}
}

func (r *replayBuffer) write(p []byte) {
	if len(p) == 0 {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	size := len(r.buf)

	// Only the tail of an oversized write can be kept; the offsets still count all of it.
	if len(p) > size {
		r.total += int64(len(p) - size)
		p = p[len(p)-size:]
	}

	start := int(r.total % int64(size))
	n := copy(r.buf[start:], p)
	copy(r.buf, p[n:])
	r.total += int64(len(p))

	if r.changed != nil {
		close(r.changed)
		r.changed = nil
	}
}

// Total is the number of bytes ever written, i.e. the offset the next byte will have.
func (r *replayBuffer) Total() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.total
}

// readFrom returns the retained bytes from offset on and the offset of the first one returned,
// which is later than asked for when the bytes in between were evicted.
func (r *replayBuffer) readFrom(offset int64) ([]byte, int64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.readLocked(offset)
}

// readAndSubscribe is readFrom plus a channel closed by the next write, taken atomically so a
// write between the two cannot be missed.
func (r *replayBuffer) readAndSubscribe(offset int64) ([]byte, int64, <-chan struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()

	data, at := r.readLocked(offset)

	if r.changed == nil {
		r.changed = make(chan struct{})
	}

	return data, at, r.changed
}

func (r *replayBuffer) readLocked(offset int64) ([]byte, int64) {
	if offset < 0 {
		offset = 0
	}

	if offset >= r.total {
		return nil, r.total
	}

	size := int64(len(r.buf))
	if oldest := r.total - size; offset < oldest {
		offset = oldest
	}

	count := int(r.total - offset)
	out := make([]byte, count)
	start := int(offset % size)
	n := copy(out, r.buf[start:])
	copy(out[n:], r.buf[:count-n])

	return out, offset
}
