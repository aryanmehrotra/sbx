package tui

import (
	"strings"
	"testing"
)

// An arrow key is an escape sequence, and Escape on its own is the same first byte. Reading
// them wrong is what makes a TUI feel broken: the cursor does not move, or the program hangs
// after Escape waiting for a sequence that was never coming.
func TestDecodingKeys(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want Key
	}{
		{"up", []byte{27, '[', 'A'}, Key{Code: KeyUp}},
		{"down", []byte{27, '[', 'B'}, Key{Code: KeyDown}},
		{"right", []byte{27, '[', 'C'}, Key{Code: KeyRight}},
		{"left", []byte{27, '[', 'D'}, Key{Code: KeyLeft}},
		{"application-mode up", []byte{27, 'O', 'A'}, Key{Code: KeyUp}},
		{"escape alone", []byte{27}, Key{Code: KeyEscape}},
		{"enter", []byte{13}, Key{Code: KeyEnter}},
		{"newline is enter too", []byte{10}, Key{Code: KeyEnter}},
		{"ctrl-c", []byte{3}, Key{Code: KeyCtrlC}},
		{"an ordinary letter", []byte{'q'}, Key{Rune: 'q', Code: KeyRune}},
		{"page up", []byte{27, '[', '5', '~'}, Key{Code: KeyPageUp}},
		{"home", []byte{27, '[', 'H'}, Key{Code: KeyHome}},
		{"nothing", nil, Key{}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := decode(c.in)
			if got != c.want {
				t.Errorf("decode(%v) = %+v, want %+v", c.in, got, c.want)
			}
		})
	}
}

// A truncated sequence must not be mistaken for a movement key, and must not hang.
func TestATruncatedSequenceIsEscape(t *testing.T) {
	for _, in := range [][]byte{{27, '['}, {27, 'O'}, {27, '[', 'Z'}} {
		if got := decode(in); got.Code != KeyEscape {
			t.Errorf("decode(%v) = %+v, want Escape", in, got)
		}
	}
}

// Several keys can arrive in one read: a held-down arrow, a paste, or two keys pressed inside
// the window the terminal waits before returning. Decoding only the first and discarding the
// rest is what made the dashboard feel unreliable - two arrows and an enter registered the
// arrows and lost the enter.
func TestEveryKeyInOneReadIsDelivered(t *testing.T) {
	in := strings.NewReader("\x1b[B\x1b[B\r")

	r := NewReader(in)

	var got []Key

	for range 3 {
		k, ok := r.Read()
		if !ok {
			break
		}

		got = append(got, k)
	}

	want := []Key{{Code: KeyDown}, {Code: KeyDown}, {Code: KeyEnter}}

	if len(got) != len(want) {
		t.Fatalf("read %d keys from one buffer, want %d: %+v", len(got), len(want), got)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Errorf("key %d is %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestConsumingOneKeyAtATime(t *testing.T) {
	cases := []struct {
		in   string
		want Key
		used int
	}{
		{"q", Key{Rune: 'q', Code: KeyRune}, 1},
		{"\x1b[A", Key{Code: KeyUp}, 3},
		{"\x1b[5~", Key{Code: KeyPageUp}, 4},
		{"\x1b", Key{Code: KeyEscape}, 1},
		{"\x1b[", Key{Code: KeyEscape}, 1},
	}

	for _, c := range cases {
		got, used := decodeOne([]byte(c.in))

		if got != c.want || used != c.used {
			t.Errorf("decodeOne(%q) = %+v/%d, want %+v/%d", c.in, got, used, c.want, c.used)
		}
	}
}

// A page-up is four bytes, and consuming three would leave a stray ~ to be read as a
// character - so the next keypress is a tilde nobody typed.
func TestATildeSequenceDoesNotLeaveItsTail(t *testing.T) {
	r := NewReader(strings.NewReader("\x1b[5~q"))

	if k, _ := r.Read(); k.Code != KeyPageUp {
		t.Fatalf("first key is %+v, want PageUp", k)
	}

	k, ok := r.Read()
	if !ok || k.Rune != 'q' {
		t.Errorf("second key is %+v (ok=%v), want q - the ~ was left in the buffer", k, ok)
	}
}

func TestTabIsAKeyOfItsOwn(t *testing.T) {
	if got := decode([]byte{9}); got.Code != KeyTab {
		t.Errorf("decode(tab) = %+v, want KeyTab - without it, tab arrives as an ordinary "+
			"character and moves nothing", got)
	}
}
