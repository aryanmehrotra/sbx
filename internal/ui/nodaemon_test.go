package ui

import (
	"strings"
	"testing"
)

// The warning is about THIS machine's daemon, so it may only be shown for a fleet this
// machine's daemon fronts.
//
// A kubernetes fleet is woken by the in-cluster activator and a deployment reached over
// `sbx connect` by the sbx running there; in both, `sbx serve` here says nothing, and the line
// would be a lie. cli.List() guards on the same fact through isLocal(); both ui paths only
// checked --connect and missed the cluster.
func TestLocalFleetIsOnlyTrueForAddressesThisDaemonOwns(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows []row
		want bool
	}{
		{"docker", []row{{Address: "127.0.0.1:20060"}}, true},
		{"kubernetes", []row{{Address: "pg.sbx.svc.cluster.local:5432"}}, false},
		{"empty fleet", nil, false},
		{"no address", []row{{Address: ""}}, false},
		// A host that merely starts with the same digits is not loopback.
		{"lookalike host", []row{{Address: "127.0.0.100:20060"}}, false},
	} {
		if got := localFleet(tc.rows); got != tc.want {
			t.Errorf("%s: localFleet = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The title has to reflect the CURRENT answer, not the one from when the dashboard opened.
//
// `sbx ui` is the long-lived surface - `sbx list` is a fresh process every time and cannot go
// stale - so a daemon that dies mid-session is exactly the case the warning exists for, and
// computing it once reproduced the original bug for it.
func TestTheTitleTracksTheDaemonChangingWhileTheDashboardIsOpen(t *testing.T) {
	m := model{version: "dev"}

	if strings.Contains(title(m, 200), "no sbx serve") {
		t.Fatal("warned before anything said the daemon was gone")
	}

	// What refresh() does on its next tick when the daemon has died.
	m.noDaemon = true

	if !strings.Contains(title(m, 200), "no sbx serve") {
		t.Error("the daemon died and the title still claims the addresses are being fronted")
	}

	// And back again, because a warning that never clears is its own wrong answer.
	m.noDaemon = false

	if strings.Contains(title(m, 200), "no sbx serve") {
		t.Error("a daemon was started and the stale warning did not clear")
	}
}
