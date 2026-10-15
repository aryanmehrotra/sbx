package osb

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/credvault"
	"github.com/aryanmehrotra/sbx/internal/egress"
)

// serverWithVaults is a server with a credential registry and a state directory of its own.
func serverWithVaults(t *testing.T) *Server {
	t.Helper()

	return &Server{credentials: credvault.NewRegistry(), store: store{dir: t.TempDir()}}
}

// policyAllowing is a normalized policy that permits these targets.
func policyAllowing(t *testing.T, targets ...string) *egress.Policy {
	t.Helper()

	p := egress.Policy{DefaultAction: "deny"}
	for _, target := range targets {
		p.Egress = append(p.Egress, egress.Rule{Action: "allow", Target: target})
	}

	norm, err := p.Normalize()
	if err != nil {
		t.Fatal(err)
	}

	return &norm
}

// A sandbox that asked for a credential proxy is given the certificate its filter will sign with,
// mounted read-only, before its first process runs. A trust store is read once at startup, so this
// is the only moment it can arrive.
func TestASandboxWithACredentialProxyIsGivenItsAuthority(t *testing.T) {
	s := serverWithVaults(t)

	path, env, err := s.credentialVaultCA("osb-1", true, policyAllowing(t, "api.example.com"))
	if err != nil {
		t.Fatal(err)
	}

	if path == "" {
		t.Fatal("no certificate was written, so there is nothing to mount")
	}

	pem, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(pem), "BEGIN CERTIFICATE") {
		t.Errorf("what was written is not a certificate: %q", pem)
	}

	if got := env[vaultCAEnv]; got != vaultCAPath {
		t.Errorf("%s = %q, want the container path %q", vaultCAEnv, got, vaultCAPath)
	}

	// Additive only. SSL_CERT_FILE and REQUESTS_CA_BUNDLE replace the root set, so setting
	// either to this one certificate would leave the sandbox unable to verify any other host.
	if got := env[nodeExtraCAEnv]; got != vaultCAPath {
		t.Errorf("%s = %q, want the container path", nodeExtraCAEnv, got)
	}

	for _, replacing := range []string{"SSL_CERT_FILE", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE"} {
		if v, set := env[replacing]; set {
			t.Errorf("%s was set to %q: it replaces the root set rather than adding to it, so "+
				"every https call to a host with a real certificate would start failing",
				replacing, v)
		}
	}
}

// The certificate the sandbox is given has to be the one its filter signs with. A second authority
// minted when the vault opens would leave the filter signing with something nothing in there
// trusts, which is the failure this delivery exists to avoid.
func TestTheDeliveredAuthorityIsTheOneTheVaultSignsWith(t *testing.T) {
	s := serverWithVaults(t)

	path, _, err := s.credentialVaultCA("osb-1", true, policyAllowing(t, "api.example.com"))
	if err != nil {
		t.Fatal(err)
	}

	delivered, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// The daemon opens the vault later, when the filter comes up, with its own live policy.
	pol := policyAllowing(t, "api.example.com")
	v := s.credentials.Open("osb-1", func() (egress.Policy, bool) { return *pol, true })

	ca := v.CA()
	if ca == nil {
		t.Fatal("the vault has no authority, so nothing can be signed")
	}

	if string(ca.PEM()) != string(delivered) {
		t.Error("the vault signs with a different authority than the sandbox was given: every " +
			"terminated tunnel would fail its handshake, and nothing would say why")
	}
}

// A sandbox that did not ask for a credential proxy gets nothing. A certificate in the trust store
// of a sandbox that can never have a vault is an authority nothing will ever sign with.
func TestASandboxWithoutACredentialProxyIsGivenNothing(t *testing.T) {
	s := serverWithVaults(t)

	path, env, err := s.credentialVaultCA("osb-1", false, policyAllowing(t, "api.example.com"))
	if err != nil {
		t.Fatal(err)
	}

	if path != "" || len(env) != 0 {
		t.Errorf("a sandbox without credentialProxy was given %q and %v", path, env)
	}
}

// No policy means no filter, and no filter means nothing to terminate. A policy of addresses alone
// has no name for a leaf to be the subject of.
func TestNothingIsDeliveredWithoutANameToSignFor(t *testing.T) {
	s := serverWithVaults(t)

	for _, c := range []struct {
		name string
		pol  *egress.Policy
	}{
		{"no policy at all", nil},
		{"addresses only", policyAllowing(t, "10.0.0.0/8", "192.0.2.7")},
	} {
		path, env, err := s.credentialVaultCA("osb-"+c.name, true, c.pol)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}

		if path != "" || len(env) != 0 {
			t.Errorf("%s: delivered %q and %v, want nothing", c.name, path, env)
		}
	}
}

// Asked twice, the same certificate: a provision that retries must not hand out a second
// authority, because the sandbox can only trust one.
func TestDeliveringTwiceGivesTheSameAuthority(t *testing.T) {
	s := serverWithVaults(t)
	pol := policyAllowing(t, "api.example.com")

	first, _, err := s.credentialVaultCA("osb-1", true, pol)
	if err != nil {
		t.Fatal(err)
	}

	before, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.credentialVaultCA("osb-1", true, pol); err != nil {
		t.Fatal(err)
	}

	after, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}

	if string(before) != string(after) {
		t.Error("a second call minted a new authority: whichever one the sandbox trusts, the " +
			"other is the one being signed with half the time")
	}
}

// The certificate goes when the sandbox does. One file per sandbox ever created would otherwise
// accumulate beside the records.
func TestRemovingASandboxTakesItsAuthorityFile(t *testing.T) {
	s := serverWithVaults(t)

	path, _, err := s.credentialVaultCA("osb-1", true, policyAllowing(t, "api.example.com"))
	if err != nil {
		t.Fatal(err)
	}

	s.removeCredentialVaultCA("osb-1")

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("%s is still there after the sandbox was removed (%v)", filepath.Base(path), err)
	}
}
