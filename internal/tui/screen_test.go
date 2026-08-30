//go:build darwin || freebsd || netbsd || openbsd || dragonfly || linux

package tui

import (
	"bytes"
	"strings"
	"testing"
)

// The dashboard must never scroll its own screen.
//
// This is the mechanism behind the fault that made it worth writing down: the screen filling
// with stacked copies of the header, each redraw one line further down, never recovering. A
// frame is drawn from the top-left with no clear in between, so it overwrites the last one
// exactly - unless writing it moves the terminal down by a line. Two things do that: a line
// wider than the terminal, which wraps and costs a row; and a frame with more lines than the
// terminal has rows. Either way the last "\n" is written on the bottom row, the terminal
// scrolls, and every frame after it is drawn one row lower than the one it was meant to
// replace.
//
// So: never more lines than rows, and autowrap off so an over-wide line is dropped rather than
// folded onto the next one.
func TestAFrameNeverScrollsTheScreen(t *testing.T) {
	var out bytes.Buffer

	s := &Screen{out: &out, Rows: 5, Cols: 20}

	tall := strings.Repeat("x\n", 20)
	s.Draw(tall)

	if got := strings.Count(out.String(), "\n"); got > s.Rows-1 {
		t.Errorf("a %d-line frame on a %d-row screen wrote %d newlines; the bottom one scrolls "+
			"the terminal and every frame after it lands a row lower",
			strings.Count(tall, "\n")+1, s.Rows, got)
	}
}

// A frame that exactly fills the screen is not trimmed - the last row is a real row, and the
// footer lives on it.
func TestAFrameThatExactlyFitsIsDrawnWhole(t *testing.T) {
	var out bytes.Buffer

	s := &Screen{out: &out, Rows: 3, Cols: 20}

	s.Draw("one\ntwo\nthree")

	for _, want := range []string{"one", "two", "three"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("a frame the height of the screen lost %q", want)
		}
	}
}

// Rows is unset when nothing has measured the terminal yet. Trimming to zero would blank the
// dashboard rather than protect it.
func TestAnUnmeasuredScreenIsNotTrimmedToNothing(t *testing.T) {
	var out bytes.Buffer

	s := &Screen{out: &out}

	s.Draw("one\ntwo\nthree")

	if !strings.Contains(out.String(), "three") {
		t.Error("a screen with no known height dropped the frame instead of drawing it")
	}
}
