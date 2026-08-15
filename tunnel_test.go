package main

import (
	"strings"
	"testing"
)

// The failure this guards against: ngrok is installed but has no authtoken, it exits
// instantly, and the tool quietly falls through to routing the user's traffic through an
// anonymous third party. Failing toward less trust is the wrong direction for a default.
func TestExplicitBackendsAreNeverAutomatic(t *testing.T) {
	auto, err := pickTunnels("")
	if err != nil {
		// No tunnel installed at all is a legitimate outcome on a bare machine; the
		// property under test is what it does NOT pick.
		if !strings.Contains(err.Error(), "no tunnel available") {
			t.Fatalf("unexpected error: %v", err)
		}

		return
	}

	for _, b := range auto {
		if b.explicit {
			t.Errorf("backend %q is explicit but was selected automatically", b.name)
		}
	}
}

// Asking for it by name is consent, and has to keep working.
func TestExplicitBackendIsAvailableWhenNamed(t *testing.T) {
	got, err := pickTunnels("ssh")
	if err != nil {
		t.Skipf("ssh not installed here: %v", err)
	}

	if len(got) != 1 || got[0].name != "ssh" {
		t.Fatalf("--via ssh selected %v, want exactly [ssh]", names(got))
	}
}

// accept-new trusts whatever answers on first contact, which is the thing host key
// checking exists to prevent — and it did it inside an automatic fallback, where nobody
// was watching.
func TestNoBackendAutoAcceptsHostKeys(t *testing.T) {
	for _, b := range tunnelBackends() {
		if b.bin != "ssh" {
			continue
		}

		args := strings.Join(b.args(12345), " ")

		if strings.Contains(args, "accept-new") {
			t.Errorf("backend %q still auto-accepts host keys: %s", b.name, args)
		}

		if !strings.Contains(args, "StrictHostKeyChecking=yes") {
			t.Errorf("backend %q does not enforce host key checking: %s", b.name, args)
		}
	}
}

// A backend that cannot be pinned must say so rather than imply it was verified.
func TestUnpinnableBackendSaysSo(t *testing.T) {
	for _, b := range tunnelBackends() {
		if b.name != "ssh" {
			continue
		}

		if b.hostKey != "" {
			t.Skip("a fingerprint is now pinned; this test is about the case where none exists")
		}

		if !strings.Contains(b.note, "no published") {
			t.Errorf("backend %q pins nothing and does not say so: %q", b.name, b.note)
		}
	}
}

func names(bs []tunnelBackend) []string {
	out := make([]string, 0, len(bs))
	for _, b := range bs {
		out = append(out, b.name)
	}

	return out
}
