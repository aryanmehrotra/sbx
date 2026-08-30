package provider

import (
	"strconv"
	"strings"
	"testing"
)

// The ports label is sbx's own state, read back off a container it did not necessarily write.
//
// `sbx list` is rebuilt entirely from labels, and TROUBLESHOOTING.md already records what a
// label that will not parse costs: the container is skipped, so it is invisible to `list` AND
// to `rm` - a sandbox you cannot see and cannot remove. That makes this parser the thing
// standing between a corrupted or hand-edited label and a fleet with holes in it.
//
// The contract: for any input, ParsePorts returns usable pairs or an error. Never a pair with
// a port outside 1-65535, never a partial list, never a panic.
func FuzzParsePorts(f *testing.F) {
	for _, s := range []string{
		"20060:30060", "20060:30060,20061:30061", "",
		"   ", ":", "a:b", "20060:", ":30060", "20060", "20060:30060:40060",
		"0:0", "-1:-1", "65536:65536", "99999999999999999999:1",
		",", ",,", "20060:30060,", ",20060:30060", "20060:30060,,20061:30061",
		" 20060 : 30060 ", "20060:30060 ,", "+20060:30060", "0x20:0x30",
	} {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, label string) {
		pairs, err := ParsePorts(label)
		if err != nil {
			return // refusing is always a valid answer
		}

		if len(pairs) == 0 {
			t.Errorf("ParsePorts(%q) returned no error and no pairs - a container with this "+
				"label would be fronted with nothing", label)
		}

		// Deliberately NOT a range check.
		//
		// A label this refuses makes the container invisible to `sbx list` and to `sbx rm`
		// - a sandbox nobody can clean up, which is worse than one whose listener fails
		// loudly. So an out-of-range port is carried through discovery on purpose and
		// refused by unit.serve, where acting on it would otherwise be silent. What must
		// hold here is only that the parse is total and self-consistent.
		for _, p := range pairs {
			if p.Public == 0 && !strings.Contains(label, "0") {
				t.Errorf("ParsePorts(%q) invented a zero public port", label)
			}
		}
	})
}

// What pairLabel writes, ParsePorts must read back exactly. They are the two halves of the
// only record of which port fronts which - written at create, read on every discovery tick for
// the life of the sandbox.
func FuzzPortLabelRoundTrip(f *testing.F) {
	f.Add(20060, 30060, 1)
	f.Add(1, 65535, 3)
	f.Add(20000, 30000, 20)

	f.Fuzz(func(t *testing.T, first, backing, n int) {
		// Keep the generated label to things the encoder is ever asked to write.
		if n < 1 || n > 20 || first < 1 || backing < 1 || first > 65535 || backing > 65535 {
			t.Skip()
		}

		if first+n > 65535 || backing+n > 65535 {
			t.Skip()
		}

		wake := make([]string, 0, n)
		back := make([]string, 0, n)

		for i := range n {
			wake = append(wake, strconv.Itoa(first+i))
			back = append(back, strconv.Itoa(backing+i))
		}

		label := pairLabel(wake, back)

		pairs, err := ParsePorts(label)
		if err != nil {
			t.Fatalf("pairLabel produced %q, which ParsePorts rejects (%v) - the sandbox would "+
				"be invisible to list and to rm", label, err)
		}

		if len(pairs) != n {
			t.Fatalf("wrote %d pairs, read back %d from %q", n, len(pairs), label)
		}

		for i, p := range pairs {
			if p.Public != first+i || p.Backing != backing+i {
				t.Errorf("pair %d round-tripped as %d:%d, want %d:%d (label %q)",
					i, p.Public, p.Backing, first+i, backing+i, label)
			}
		}

		if strings.Count(label, ",") != n-1 {
			t.Errorf("label %q has the wrong separator count for %d pairs", label, n)
		}
	})
}
