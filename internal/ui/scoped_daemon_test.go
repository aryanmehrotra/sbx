package ui

import (
	"testing"

	"github.com/aryanmehrotra/sbx/internal/daemon"
)

// The dashboard's "no sbx serve" line must not fire for sandboxes a daemon started with --only
// is fronting, and must still fire for one outside every scope.
func TestDaemonServesFindsAScopedDaemon(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	scope, _ := daemon.ParseScope([]string{"osb-"})
	defer daemon.Announce("docker", scope)()

	in := []row{{Sandbox: "osb-a", Address: "127.0.0.1:20000"}, {Sandbox: "osb-b", Address: "127.0.0.1:20010"}}
	if !daemonServes(in) {
		t.Error("rows all inside a running daemon's --only read as unserved")
	}

	mixed := append(in, row{Sandbox: "zopnight", Address: "127.0.0.1:20020"})
	if daemonServes(mixed) {
		t.Error("a row outside every daemon's scope read as served")
	}
}
