package tunnel

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
// checking exists to prevent - and it did it inside an automatic fallback, where nobody
// was watching.
func TestNoBackendAutoAcceptsHostKeys(t *testing.T) {
	for _, b := range tunnelBackends() {
		if b.bin != "ssh" {
			continue
		}

		args := strings.Join(b.args(12345, false), " ")

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

// cloudflared's control plane must never be mistaken for the tunnel.
//
// cloudflared logs a call to api.trycloudflare.com BEFORE it prints the tunnel
// hostname, so matching the first hit handed the user a URL that answers 405
// from Cloudflare's API and reaches no sandbox at all. Seen live.
func TestCloudflaredControlPlaneIsNotTheTunnelURL(t *testing.T) {
	var b *tunnelBackend

	all := tunnelBackends()
	for i := range all {
		if all[i].name == "cloudflared" {
			b = &all[i]
			break
		}
	}

	if b == nil {
		t.Skip("no cloudflared backend registered")
	}

	// The order cloudflared really prints them in.
	lines := []string{
		`INF Requesting new quick Tunnel on trycloudflare.com...`,
		`INF Connecting to https://api.trycloudflare.com to request a tunnel`,
		`INF |  https://operators-authors-opponent-exchanges.trycloudflare.com  |`,
	}

	var got string

	for _, line := range lines {
		if m := b.url.FindStringSubmatch(line); m != nil && !b.notURL[m[len(m)-1]] {
			got = m[len(m)-1]
			break
		}
	}

	if want := "https://operators-authors-opponent-exchanges.trycloudflare.com"; got != want {
		t.Errorf("picked %q, want %q", got, want)
	}
}

// cloudflared's metrics listener takes the first free port of 20241-20245, and sbx hands out
// 20000-21199 - so the default lands inside slot 12 and the daemon can no longer bind it.
// Measured before the fix: cloudflared LISTEN on 127.0.0.1:20241, and net.Listen on it failed
// with "address already in use". Pinned here because the collision is invisible until the day
// slot 12 is the one being allocated.
func TestCloudflaredDoesNotBindInsideTheSandboxPortRange(t *testing.T) {
	for _, b := range tunnelBackends() {
		if b.name != "cloudflared" {
			continue
		}

		args := strings.Join(b.args(20060, true), " ")

		if !strings.Contains(args, "--metrics 127.0.0.1:0") {
			t.Errorf("cloudflared is started without --metrics 127.0.0.1:0, so its metrics "+
				"server can take a port sbx allocates (20000-21199): %s", args)
		}
	}
}

// The Host a dev server is sent decides whether it serves the page at all. Measured against a
// real vite: the tunnel hostname answers 403 "Blocked request", 127.0.0.1:<port> answers 200.
func TestHostHeaderRewriteIsAskedForOnlyWhereItWorks(t *testing.T) {
	for _, b := range tunnelBackends() {
		with := strings.Join(b.args(20060, true), " ")
		without := strings.Join(b.args(20060, false), " ")

		switch b.name {
		case "cloudflared":
			if !b.hostRewrite {
				t.Error("cloudflared has --http-host-header; hostRewrite says otherwise")
			}

			if !strings.Contains(with, "--http-host-header 127.0.0.1:20060") {
				t.Errorf("no host rewrite where one was asked for: %s", with)
			}

			if strings.Contains(without, "--http-host-header") {
				t.Errorf("host rewritten where it was not asked for: %s", without)
			}
		default:
			// A backend that cannot rewrite must not change its command line when asked to,
			// so the honest message is the only thing that happens.
			if b.hostRewrite {
				t.Errorf("backend %q claims it can rewrite Host; only cloudflared can", b.name)
			}

			if with != without {
				t.Errorf("backend %q changed its arguments for a rewrite it cannot do:\n"+
					" with: %s\n without: %s", b.name, with, without)
			}
		}
	}
}

// The default must not take a working command away.
//
// --host-header defaults to rewrite and ssh can never rewrite, so a check that fired on the
// value rather than on the ASK refused to run `sbx url x web --via ssh` at all - the form that
// works with nothing installed, which is the only reason the ssh backend is in the list. ssh
// is explicit-only, so --via ssh is the sole way to reach it and it was broken outright.
func TestADefaultedHostHeaderDoesNotRefuseABackendThatCannotRewrite(t *testing.T) {
	for _, name := range []string{"ssh", "ngrok"} {
		var chosen []tunnelBackend

		for _, b := range tunnelBackends() {
			if b.name == name {
				chosen = append(chosen, b)
			}
		}

		// "" is what the CLI passes when the flag was never typed.
		if err := refuseHostRewrite(chosen, name, ""); err != nil {
			t.Errorf("--via %s refused under a defaulted --host-header, so the command does not "+
				"run at all: %v", name, err)
		}

		// pass is an explicit "do not rewrite", so it can never be a conflict.
		if err := refuseHostRewrite(chosen, name, "pass"); err != nil {
			t.Errorf("--via %s --host-header pass refused, and pass is the escape hatch: %v", name, err)
		}
	}
}

// Typing it is different from defaulting to it: an explicit ask for something a named backend
// cannot do is still an error, because silently sending the public hostname is the shape this
// project keeps finding - reported success, reached nothing.
func TestAnExplicitHostHeaderRewriteIsRefusedWhereItCannotWork(t *testing.T) {
	for _, name := range []string{"ssh", "ngrok"} {
		var chosen []tunnelBackend

		for _, b := range tunnelBackends() {
			if b.name == name {
				chosen = append(chosen, b)
			}
		}

		err := refuseHostRewrite(chosen, name, "rewrite")
		if err == nil {
			t.Errorf("--via %s --host-header rewrite was accepted, and the Host would silently "+
				"not be rewritten", name)

			continue
		}

		if !strings.Contains(err.Error(), "cloudflared") {
			t.Errorf("the refusal does not name the backend that can do it: %v", err)
		}
	}
}

// cloudflared can rewrite, so nothing is ever refused for it.
func TestCloudflaredIsNeverRefusedAHostRewrite(t *testing.T) {
	var chosen []tunnelBackend

	for _, b := range tunnelBackends() {
		if b.name == "cloudflared" {
			chosen = append(chosen, b)
		}
	}

	for _, h := range []string{"", "rewrite", "pass"} {
		if err := refuseHostRewrite(chosen, "cloudflared", h); err != nil {
			t.Errorf("cloudflared refused with --host-header %q: %v", h, err)
		}
	}
}

// Where sbx picked the backend rather than being told, falling to one that cannot rewrite is a
// downgrade to announce, not an error - runTunnel prints that line.
func TestAnUnnamedBackendIsNeverRefused(t *testing.T) {
	if err := refuseHostRewrite(tunnelBackends(), "", "rewrite"); err != nil {
		t.Errorf("refused a backend the user never named: %v", err)
	}
}
