//go:build darwin || freebsd || netbsd || openbsd || dragonfly || linux

package tui

// The drawing half: an alternate screen, and frames written in one syscall.
//
// One write per frame, not one per line. A dashboard that writes its rows individually tears
// visibly on a slow terminal - the top of the table is the new frame while the bottom is
// still the old one - and over ssh it is worse. Building the whole frame in a buffer and
// writing it once is both faster and the reason it looks steady.

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
)

// ANSI, written out rather than pulled from a library. There are six of them.
const (
	altScreenOn  = "\x1b[?1049h"
	altScreenOff = "\x1b[?1049l"
	cursorHide   = "\x1b[?25l"
	cursorShow   = "\x1b[?25h"
	clear        = "\x1b[2J"
	home         = "\x1b[H"
)

// Screen owns the terminal for as long as a dashboard is running.
type Screen struct {
	out io.Writer
	fd  uintptr

	mu    sync.Mutex
	saved *state

	// last is the frame currently on the terminal, and dirty forces the next write even if
	// the frame matches it.
	last  string
	dirty bool

	// suspended is true between giving the terminal back for a ^Z and taking it again, so
	// that taking it twice does not save a raw terminal as the state to restore at exit.
	suspended bool

	Rows, Cols int

	// Resized is signalled when the window changes, so the caller can redraw immediately
	// rather than at the next tick.
	Resized chan struct{}

	stopSignals chan os.Signal
	contSignals chan os.Signal
	restoreOnce sync.Once
}

// Open takes over the terminal. The returned Close must run, and callers should defer it
// before anything that can panic: a terminal left in raw mode with no cursor is one somebody
// has to blindly type `reset` into.
func Open(f *os.File) (*Screen, error) {
	if !supported {
		return nil, ErrUnsupported
	}

	if !IsTerminal(f) {
		return nil, ErrUnsupported
	}

	saved, err := makeRaw(f.Fd())
	if err != nil {
		return nil, fmt.Errorf("could not put the terminal into raw mode: %w", err)
	}

	s := &Screen{
		out:     f,
		fd:      f.Fd(),
		saved:   saved,
		Resized: make(chan struct{}, 1),
	}

	s.Rows, s.Cols = size(s.fd)

	fmt.Fprint(s.out, altScreenOn+cursorHide+clear+home)

	s.watchResize()
	s.watchSignals(f)
	s.watchResume()

	return s, nil
}

// watchResize keeps Rows and Cols current and nudges the caller to redraw.
func (s *Screen) watchResize() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)

	go func() {
		for range ch {
			s.mu.Lock()
			s.Rows, s.Cols = size(s.fd)
			s.dirty = true // the terminal moved under us; what is on it cannot be trusted
			s.mu.Unlock()

			select {
			case s.Resized <- struct{}{}:
			default: // a redraw is already pending; one is enough
			}
		}
	}()
}

// watchSignals puts the terminal back if the process is killed.
//
// Raw mode turns off the terminal's own signal generation, so ^C arrives as a byte rather
// than as SIGINT and the caller handles it. This is for the signals that still arrive:
// SIGTERM from a supervisor, SIGHUP when the ssh session drops. Without it the terminal is
// left unusable by something the user did not do.
func (s *Screen) watchSignals(f *os.File) {
	s.stopSignals = make(chan os.Signal, 1)
	signal.Notify(s.stopSignals, syscall.SIGTERM, syscall.SIGHUP)

	go func() {
		if _, ok := <-s.stopSignals; !ok {
			return // Close ran first, which is the ordinary path
		}

		s.putBack()
		os.Exit(1)
	}()
}

// watchResume takes the terminal back when the process is continued.
//
// Suspend already does this on the way out of its own ^Z, so in the ordinary path this is a
// second, harmless re-assertion. It is here for the stop this program did not ask for - a
// `kill -STOP` from another terminal, or a ^Z delivered while the terminal was briefly not
// raw - after which the alternate screen and raw mode have to be put back by somebody, and
// there is nobody else.
func (s *Screen) watchResume() {
	s.contSignals = make(chan os.Signal, 1)
	signal.Notify(s.contSignals, syscall.SIGCONT)

	go func() {
		for range s.contSignals {
			s.acquire()
		}
	}()
}

// Suspend gives the terminal back, stops this process the way ^Z is meant to, and takes the
// terminal again when the shell brings it back to the foreground.
//
// Raw mode turns the terminal's own signal generation off, so ^Z never becomes SIGTSTP: it
// arrives as byte 26 and the caller routes it here. Before this the key did nothing at all,
// and a stop that arrived from anywhere else left the process suspended with the alternate
// screen still up and the terminal still raw - which hands the shell back a screen it cannot
// draw a prompt on, and a user who has to blindly type `reset`.
func (s *Screen) Suspend() {
	s.release()

	// Raised with its default disposition - nothing here notifies SIGTSTP - so this stops the
	// process rather than being delivered to a handler. It returns when somebody runs fg.
	_ = syscall.Kill(syscall.Getpid(), syscall.SIGTSTP)

	s.acquire()
}

// release hands the terminal back exactly as putBack does, but repeatably: Close is once and
// final, this is one half of a pair that runs every time somebody suspends.
func (s *Screen) release() {
	s.mu.Lock()

	if s.suspended {
		s.mu.Unlock()

		return
	}

	s.suspended, s.last = true, ""
	saved := s.saved

	s.mu.Unlock()

	fmt.Fprint(s.out, altScreenOff+cursorShow)

	_ = restore(s.fd, saved)
}

// acquire takes the terminal back: raw mode, the alternate screen, and a forced redraw of
// whatever the model says now rather than whatever was on screen a suspension ago.
func (s *Screen) acquire() {
	s.mu.Lock()
	wasSuspended := s.suspended
	s.suspended = false
	s.mu.Unlock()

	st, err := makeRaw(s.fd)

	// Only when the terminal was known to be cooked. makeRaw reports what it found, and if
	// this is the second acquire of one resume, what it found was raw - keeping that as the
	// state to restore at exit would give the user's shell back in raw mode.
	if err == nil && wasSuspended {
		s.mu.Lock()
		s.saved = st
		s.mu.Unlock()
	}

	fmt.Fprint(s.out, altScreenOn+cursorHide+clear+home)

	s.mu.Lock()
	s.Rows, s.Cols = size(s.fd)
	s.mu.Unlock()

	// The window may well have been resized while this program was not running to notice.
	s.Redraw()
}

// Size reports the current terminal size, safely against the resize watcher.
func (s *Screen) Size() (rows, cols int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.Rows, s.Cols
}

// Draw replaces the screen with frame, in one write - and only if it differs from what is
// already there.
//
// The caller redraws on a timer, ten times a second, so that a keypress is never waiting on a
// poll. Three quarters of those frames were byte-identical to the one already on screen, and
// writing them anyway meant repainting a full terminal 76 KB a second for nothing. The cost
// does not show up in this process's CPU - it lands in the terminal emulator, as a shimmer
// that makes a still screen look unsteady and a scrolling one look worse.
//
// Skipping the write is safe precisely because the frame is a pure function of the model: if
// the string has not changed, neither has anything the reader can see.
func (s *Screen) Draw(frame string) {
	s.mu.Lock()

	if frame == s.last && !s.dirty {
		s.mu.Unlock()

		return
	}

	s.last, s.dirty = frame, false

	s.mu.Unlock()

	var b bytes.Buffer

	b.WriteString(home)

	// Clearing each line as it is drawn, rather than clearing the screen first, is what stops
	// the flicker: a full clear followed by a redraw shows an empty screen for one frame.
	for i, line := range strings.Split(frame, "\n") {
		if i > 0 {
			b.WriteString("\n")
		}

		b.WriteString("\x1b[2K") // clear to end of line
		b.WriteString(line)
	}

	b.WriteString("\x1b[0J") // and clear anything below a frame that got shorter

	_, _ = s.out.Write(b.Bytes())
}

// Redraw forces the next Draw to write even if the frame is unchanged. Used when something
// other than this program has touched the terminal - a resize, a resume from suspend - and
// what is on screen can no longer be trusted to match what was last drawn.
func (s *Screen) Redraw() {
	s.mu.Lock()
	s.dirty = true
	s.mu.Unlock()
}

// Close gives the terminal back exactly as it was found.
func (s *Screen) Close() {
	s.putBack()
}

func (s *Screen) putBack() {
	s.restoreOnce.Do(func() {
		signal.Stop(s.stopSignals)
		signal.Stop(s.contSignals)

		fmt.Fprint(s.out, altScreenOff+cursorShow)

		s.mu.Lock()
		saved := s.saved
		s.mu.Unlock()

		_ = restore(s.fd, saved)
	})
}
