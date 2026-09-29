package cli

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// A frozen service (on_idle: freeze, container paused) holds its memory and processes; a stopped
// one holds nothing. `sbx list` called both "asleep", so the one question a freeze user asks -
// is it keeping my RAM - had no answer. The table says "frozen"; JSON adds "state" and keeps
// "awake" exactly as it was, so existing `select(.awake)` callers do not change.
func TestListShowsFrozenApartFromAsleep(t *testing.T) {
	units := []provider.Unit{
		{Sandbox: "nb", Service: "a-up", Running: true},
		{Sandbox: "nb", Service: "b-stopped"},
		{Sandbox: "nb", Service: "c-frozen", Paused: true, OnIdle: "freeze"},
	}

	for _, c := range []struct {
		i    int
		want string
	}{{0, "awake"}, {1, "asleep"}, {2, "frozen"}} {
		if got := unitState(units[c.i]); got != c.want {
			t.Errorf("%s: state %q, want %q", units[c.i].Service, got, c.want)
		}
	}

	var buf bytes.Buffer
	if err := listJSON(&buf, units, "docker"); err != nil {
		t.Fatal(err)
	}

	var got []struct {
		Service string `json:"service"`
		Awake   bool   `json:"awake"`
		State   string `json:"state"`
	}

	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}

	want := map[string]struct {
		awake bool
		state string
	}{"a-up": {true, "awake"}, "b-stopped": {false, "asleep"}, "c-frozen": {false, "frozen"}}

	for _, g := range got {
		w := want[g.Service]
		if g.Awake != w.awake || g.State != w.state {
			t.Errorf("%s = awake %v state %q, want awake %v state %q", g.Service, g.Awake, g.State, w.awake, w.state)
		}
	}
}
