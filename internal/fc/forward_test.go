package fc

import (
	"errors"
	"testing"
)

func TestBridgeIsolationIsCheckedNotAssumed(t *testing.T) {
	rules := func(s string, err error) func() (string, error) {
		return func() (string, error) { return s, err }
	}

	for _, tc := range []struct {
		name     string
		fwd      string
		rules    func() (string, error)
		isolated bool
		known    bool
	}{
		{"forwarding off", "0\n", nil, true, true},
		{"docker's DROP", "1\n", rules("-P INPUT ACCEPT\n-P FORWARD DROP\n-N DOCKER\n", nil), true, true},
		{"ACCEPT", "1", rules("-P FORWARD ACCEPT\n", nil), false, true},
		{"unreadable as non-root", "1", rules("", errors.New("Permission denied (you must be root)")), false, true},
		{"no policy line", "1", rules("-N DOCKER\n", nil), false, true},
		{"not linux", "", nil, false, false},
	} {
		got := CheckBridgeIsolation(tc.fwd, tc.rules)
		if got.Isolated != tc.isolated || got.Known != tc.known || (tc.known && !got.Isolated && got.Meaning == "") {
			t.Errorf("%s: %+v", tc.name, got)
		}
	}
}
