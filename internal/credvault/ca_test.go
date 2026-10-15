package credvault

import (
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// The constraint has to be IN the certificate, not only in the code that signs with it.
//
// Leaf refuses a host no binding names, which is the check this process makes. That check is one
// bug away from not happening, so the test that earns the design is the other one: sign a leaf for
// an unnamed host with the authority's own key, deliberately going around Leaf, and have a verifier
// refuse it. If the name constraint is missing, absent, or not what it should be, that leaf
// verifies and this test fails.
func TestAVerifierRefusesALeafOutsideTheNameConstraint(t *testing.T) {
	ca, err := NewCA([]string{"api.example.com"})
	if err != nil {
		t.Fatal(err)
	}

	// The honest path: a host a binding names, verified against the authority.
	leaf, err := ca.Leaf("api.example.com")
	if err != nil {
		t.Fatalf("the authority would not sign for the host its binding names: %v", err)
	}

	if err := verify(t, ca, leaf.Leaf, "api.example.com"); err != nil {
		t.Fatalf("a leaf for the bound host did not verify: %v", err)
	}

	// The forged path: the same key, a host nothing named.
	forged := forge(t, ca, &x509.Certificate{
		Subject:  pkix.Name{CommonName: "login.microsoftonline.com"},
		DNSNames: []string{"login.microsoftonline.com"},
	})

	err = verify(t, ca, forged, "login.microsoftonline.com")
	if err == nil {
		t.Fatal("a leaf for a host outside the authority's constraints verified: the certificate " +
			"does not carry the name constraint, so this sandbox's key can sign for anything")
	}

	var bad x509.CertificateInvalidError
	if !errors.As(err, &bad) || bad.Reason != x509.CANotAuthorizedForThisName {
		t.Fatalf("the forged leaf failed for the wrong reason (%v): the name constraint is not "+
			"what refused it", err)
	}
}

// The IP form of the constraint is the one that gets left out: the DNS list looks like the whole
// answer, and a certificate silent about addresses may be signed for any of them.
func TestAVerifierRefusesALeafWithAnAddress(t *testing.T) {
	ca, err := NewCA([]string{"api.example.com"})
	if err != nil {
		t.Fatal(err)
	}

	forged := forge(t, ca, &x509.Certificate{
		Subject:     pkix.Name{CommonName: "169.254.169.254"},
		IPAddresses: []net.IP{net.ParseIP("169.254.169.254")},
	})

	err = verify(t, ca, forged, "169.254.169.254")
	if err == nil {
		t.Fatal("a leaf for the cloud metadata address verified: every IP range must be excluded, " +
			"or this authority can sign for the one address a sandbox must never be able to read")
	}

	var bad x509.CertificateInvalidError
	if !errors.As(err, &bad) || bad.Reason != x509.CANotAuthorizedForThisName {
		t.Fatalf("the forged leaf failed for the wrong reason (%v)", err)
	}
}

// A wildcard binding becomes the subdomain-only constraint, which is what the wildcard means. A
// bare host becomes the suffix form, which is the narrowest this extension can spell.
func TestTheConstraintsAreTheBoundHostsAndNothingElse(t *testing.T) {
	ca, err := NewCA([]string{"*.example.com", "Api.Other.Com.", "*.example.com"})
	if err != nil {
		t.Fatal(err)
	}

	if got := strings.Join(ca.Hosts(), ","); got != ".example.com,api.other.com" {
		t.Fatalf("constraints = %q, want \".example.com,api.other.com\" - lower-cased, the "+
			"trailing dot gone, the wildcard as a suffix and the repeat dropped", got)
	}

	cert := ca.Certificate()

	if !cert.IsCA || !cert.BasicConstraintsValid {
		t.Fatal("the authority is not a CA with valid basic constraints")
	}

	if !cert.MaxPathLenZero {
		t.Fatal("the authority may sign another authority: a second, unconstrained CA could be " +
			"minted from this key")
	}

	if !cert.PermittedDNSDomainsCritical {
		t.Fatal("the name constraint is not critical: a verifier that does not understand the " +
			"extension would ignore the limit instead of refusing the certificate")
	}

	if len(cert.ExcludedIPRanges) != 2 {
		t.Fatalf("ExcludedIPRanges = %v, want both families in full", cert.ExcludedIPRanges)
	}

	// A wildcard covers a subdomain and not the apex; a bare host covers itself.
	for host, want := range map[string]bool{
		"a.example.com": true, "example.com": false,
		"api.other.com": true, "other.com": false, "elsewhere.com": false,
	} {
		if _, err := ca.Leaf(host); (err == nil) != want {
			t.Errorf("Leaf(%q) error = %v, want signed = %v", host, err, want)
		}
	}
}

// The key dies with the sandbox, so a handshake that arrives afterwards is told so rather than
// served from a key that should no longer exist.
func TestDestroyStopsTheAuthoritySigning(t *testing.T) {
	ca, err := NewCA([]string{"api.example.com"})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := ca.Leaf("api.example.com"); err != nil {
		t.Fatal(err)
	}

	ca.Destroy()

	if _, err := ca.Leaf("api.example.com"); !errors.Is(err, ErrDestroyed) {
		t.Fatalf("Leaf after Destroy = %v, want ErrDestroyed - a cached leaf must not outlive "+
			"the key either", err)
	}
}

// An address is refused where it is written, not at the handshake: the IP constraint excludes
// every range, so a binding on one could never be served.
func TestAnAddressIsNotAHostAnAuthorityCanBeConstrainedTo(t *testing.T) {
	for _, host := range []string{"169.254.169.254", "*.169.254.169.254", "::1", ""} {
		if _, err := NewCA([]string{host}); err == nil {
			t.Errorf("NewCA(%q) was accepted; a certificate is issued for a name", host)
		}
	}

	if _, err := NewCA(nil); err == nil {
		t.Error("NewCA with no hosts was accepted: an unconstrained authority is the thing this " +
			"type exists to prevent")
	}
}

// forge signs cert with the authority's own key, going around Leaf on purpose, so a test can ask
// what a verifier makes of a certificate this code would never issue.
func forge(t *testing.T, ca *CA, tmpl *x509.Certificate) *x509.Certificate {
	t.Helper()

	serial, err := serialNumber()
	if err != nil {
		t.Fatal(err)
	}

	tmpl.SerialNumber = serial
	tmpl.NotBefore = time.Now().Add(-time.Hour)
	tmpl.NotAfter = time.Now().Add(time.Hour)
	tmpl.KeyUsage = x509.KeyUsageDigitalSignature
	tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	tmpl.BasicConstraintsValid = true

	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &ca.leafKey.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}

	got, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}

	return got
}

// verify is what a client inside the sandbox does: trust this authority and nothing else, then
// check the certificate it was handed for the name it asked for.
func verify(t *testing.T, ca *CA, leaf *x509.Certificate, host string) error {
	t.Helper()

	pool := x509.NewCertPool()
	pool.AddCert(ca.Certificate())

	_, err := leaf.Verify(x509.VerifyOptions{
		DNSName:     host,
		Roots:       pool,
		CurrentTime: time.Now(),
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})

	return err
}
