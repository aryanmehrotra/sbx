package ui

import (
	"strings"
	"testing"
)

// A dashboard whose addresses cannot answer has to say so.
//
// Every ADDRESS on the screen is the daemon's port, not docker's, so with no `sbx serve` the
// whole column refuses while the STATE column still reads AWAKE. Reported exactly that way:
// mlflow shown awake on 127.0.0.1:20020 holding 567 MB, and the browser saying "refused to
// connect". Both true, and nothing on the screen reconciled them.
func TestTitleSaysWhenNoDaemonOwnsTheAddresses(t *testing.T) {
	m := model{version: "dev", noDaemon: true}

	got := title(m, 200)

	if !strings.Contains(got, "no sbx serve") {
		t.Errorf("the title does not say the addresses are unowned:\n%s", got)
	}
}

func TestTitleIsQuietWhenADaemonIsRunning(t *testing.T) {
	m := model{version: "dev"}

	if got := title(m, 200); strings.Contains(got, "no sbx serve") {
		t.Errorf("warned about a daemon that is running:\n%s", got)
	}
}

// A deployment reached over `sbx connect` is fronted by the sbx running there, so this
// machine's daemon says nothing about it and the warning would be false.
func TestNoDaemonWarningIsNotShownForARemoteFleet(t *testing.T) {
	m := model{version: "dev", remote: true, noDaemon: false}

	if got := title(m, 200); strings.Contains(got, "no sbx serve") {
		t.Errorf("warned about a local daemon while showing a remote fleet:\n%s", got)
	}
}
