//go:build unix

package provider

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/aryanmehrotra/sbx/internal/tui"
)

// watchResize reports f's size now and on every SIGWINCH until stop is called.
func watchResize(f *os.File, fn func(rows, cols int)) (stop func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)

	done := make(chan struct{})

	go func() {
		for {
			select {
			case <-ch:
				fn(tui.Size(f))
			case <-done:
				return
			}
		}
	}()

	return func() { signal.Stop(ch); close(done) }
}
