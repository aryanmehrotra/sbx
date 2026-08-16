//go:build !(darwin || freebsd || netbsd || openbsd || dragonfly || linux)

package tui

// No screen here, for the same reason there is no raw mode: see raw_other.go. Open refuses
// and the caller prints its static view, which is what it does on a pipe too.

import "os"

type Screen struct {
	Rows, Cols int
	Resized    chan struct{}
}

func Open(*os.File) (*Screen, error) { return nil, ErrUnsupported }

func (s *Screen) Size() (int, int) { return 24, 80 }

func (s *Screen) Draw(string) {}

func (s *Screen) Close() {}
