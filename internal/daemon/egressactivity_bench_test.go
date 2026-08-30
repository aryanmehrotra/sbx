package daemon

import (
	"io"
	"sync/atomic"
	"testing"
)

// The stamp sits in the copy loop of every allow-listed byte, so its cost is the question worth
// asking before shipping it. Measured against the same copy with no hook attached.

func BenchmarkEgressCopyWithoutStamp(b *testing.B) {
	f := NewEgressFilter([]string{"x"})
	buf := make([]byte, 32*1024)

	w := f.active(io.Discard)

	b.SetBytes(int64(len(buf)))
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		_, _ = w.Write(buf)
	}
}

func BenchmarkEgressCopyWithStamp(b *testing.B) {
	var n atomic.Int64

	f := NewEgressFilter([]string{"x"})
	f.OnActivity = func() { n.Add(1) }

	buf := make([]byte, 32*1024)

	w := f.active(io.Discard)

	b.SetBytes(int64(len(buf)))
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		_, _ = w.Write(buf)
	}
}

// And the throttled walk itself, which is what the stamp defers to at most once a second.
func BenchmarkEgressDueThrottled(b *testing.B) {
	p := &egressProxy{}

	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		_ = p.due(1)
	}
}
