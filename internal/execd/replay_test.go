//go:build unix

package execd

import (
	"bytes"
	"testing"
)

func TestReplayBufferOffsetsSurviveWrapAndEviction(t *testing.T) {
	r := newReplayBuffer(8)

	r.write([]byte("abcde"))

	if d, at := r.readFrom(0); string(d) != "abcde" || at != 0 {
		t.Fatalf("from 0: %q at %d", d, at)
	}

	if d, at := r.readFrom(3); string(d) != "de" || at != 3 {
		t.Fatalf("from 3: %q at %d", d, at)
	}

	r.write([]byte("fghij")) // total 10, wraps; "ab" evicted

	if d, at := r.readFrom(0); string(d) != "cdefghij" || at != 2 {
		t.Fatalf("after wrap from 0: %q at %d, want the retained tail from offset 2", d, at)
	}

	if d, at := r.readFrom(10); d != nil || at != 10 {
		t.Fatalf("caught up: %q at %d", d, at)
	}

	r.write(bytes.Repeat([]byte("z"), 20)) // larger than the ring

	if d, at := r.readFrom(0); string(d) != "zzzzzzzz" || at != 22 || r.Total() != 30 {
		t.Fatalf("oversized write: %q at %d total %d", d, at, r.Total())
	}
}

func TestReplayBufferSubscribersWakeOnWrite(t *testing.T) {
	r := newReplayBuffer(16)

	_, _, ch1 := r.readAndSubscribe(0)
	_, _, ch2 := r.readAndSubscribe(0)

	select {
	case <-ch1:
		t.Fatal("woke before a write")
	default:
	}

	r.write([]byte("x"))

	for _, ch := range []<-chan struct{}{ch1, ch2} {
		select {
		case <-ch:
		default:
			t.Fatal("a subscriber was not woken by the write")
		}
	}
}
