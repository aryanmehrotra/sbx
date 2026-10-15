package credvault

// The per-sandbox certificate authority.
//
// Adding a credential to an HTTPS request means reading the request, which means terminating TLS,
// which means the sandbox has to trust a certificate for the host it dialled. So each sandbox gets
// its own authority, constrained to the hosts its own bindings name, and its private key is
// generated in this process and never written anywhere.
//
// One authority per sandbox and not one per machine, because a machine-wide key is a key that
// signs for every sandbox: lift it once out of one and every other sandbox on the host is
// interceptable, including by whoever lifted it. Per sandbox, the blast radius of the key is the
// sandbox it was minted for, and it stops existing when that sandbox does.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"strings"
	"sync"
	"time"
)

// caLifetime and leafLifetime bound the certificates. Both are generous next to what they protect:
// the authority exists only while the sandbox does, and a sandbox that outlives a year has bigger
// problems than a certificate. Short enough that a key somehow recovered from a core dump is not
// useful for ever.
const (
	caLifetime   = 365 * 24 * time.Hour
	leafLifetime = 30 * 24 * time.Hour

	// backdate covers clock skew between this host and the sandbox, which on a VM-backed engine
	// is a different clock. A leaf that is not yet valid fails the handshake with an error about
	// time that reads like a certificate error.
	backdate = time.Hour
)

// CA is one sandbox's authority: the certificate the sandbox trusts, the key that signs leaves for
// it, and the ceiling on what that key may sign for.
//
// The zero value is not usable; NewCA makes one. Every method is safe for concurrent use, because
// leaves are minted on the data path and the data path is every connection at once.
type CA struct {
	mu sync.Mutex

	cert *x509.Certificate
	der  []byte
	key  *ecdsa.PrivateKey

	// leafKey signs nothing; it is the key every leaf carries. One key for all of a sandbox's
	// leaves rather than one each: they all live in this process for this sandbox, so a second
	// key buys no separation, and minting a leaf on first contact with a host should cost a
	// signature rather than a key generation.
	leafKey *ecdsa.PrivateKey

	// leaves is the cache, keyed by the lower-cased host. A handshake happens on the connection
	// path and an agent talks to the same few hosts all day.
	leaves map[string]*tls.Certificate

	// hosts is what this authority is permitted to sign for, as the constraint names them, kept
	// for the message a refusal gives.
	hosts []string

	// destroyed is set by Destroy. Checked before signing, so a leaf cannot be minted from a
	// sandbox that has gone: the goroutine holding a half-finished handshake is told plainly
	// rather than signing with a key that should no longer exist.
	destroyed bool
}

// ErrDestroyed is an authority whose sandbox is gone.
var ErrDestroyed = errors.New("this sandbox's credential authority has been destroyed")

// NewCA mints an authority permitted to sign only for hosts.
//
// hosts are binding hosts: an FQDN, or a "*."-wildcard. They become the certificate's DNS name
// constraints, marked critical, and every IP range is excluded. The constraints are a ceiling the
// key cannot sign past even if the code that uses it is wrong later - which is the reason they are
// here and not only in the matching rules: the matching rules are a decision this process makes
// every request, and the constraint is a decision made once, at minting, that a verifier on the
// other side enforces.
func NewCA(hosts []string) (*CA, error) {
	permitted, err := dnsConstraints(hosts)
	if err != nil {
		return nil, err
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating this sandbox's credential authority key: %w", err)
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating this sandbox's leaf key: %w", err)
	}

	serial, err := serialNumber()
	if err != nil {
		return nil, err
	}

	now := time.Now()

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "sbx sandbox credential proxy"},
		NotBefore:    now.Add(-backdate),
		NotAfter:     now.Add(caLifetime),

		BasicConstraintsValid: true,
		IsCA:                  true,

		// A leaf and nothing else: this authority may not sign another authority, so it cannot
		// be used to mint a second, unconstrained one.
		MaxPathLen:     0,
		MaxPathLenZero: true,

		KeyUsage: x509.KeyUsageCertSign,

		// Critical, as RFC 5280 requires for name constraints: a verifier that does not
		// understand the extension must refuse the certificate rather than ignore the limit.
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         permitted,

		// Every IP range excluded, both families. Bindings name FQDNs, so nothing here ever
		// needs an address SAN - and an authority that is silent about addresses is an authority
		// permitted to sign for any of them, because an absent constraint type is unconstrained.
		// That is the form of this extension people miss: the DNS list looks like the whole
		// answer and is not.
		ExcludedIPRanges: everyIPRange(),
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("minting this sandbox's credential authority: %w", err)
	}

	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("reading back this sandbox's credential authority: %w", err)
	}

	return &CA{cert: cert, der: der, key: key, leafKey: leafKey,
		leaves: map[string]*tls.Certificate{}, hosts: permitted}, nil
}

// Certificate is the authority's own certificate, for a trust store inside the sandbox. It carries
// no private key.
func (c *CA) Certificate() *x509.Certificate {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.cert
}

// PEM is the authority's certificate in the form a trust store reads.
func (c *CA) PEM() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()

	return pemBlock("CERTIFICATE", c.der)
}

// Hosts is what this authority may sign for, as the constraints name them.
func (c *CA) Hosts() []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]string(nil), c.hosts...)
}

// Leaf returns the certificate for host, minting it on first ask.
//
// It refuses a host outside the constraints instead of signing one the sandbox would then reject
// in a handshake: the refusal names the host and what the authority covers, which is the
// difference between "that binding does not cover this host" and an unexplained TLS failure.
// Covers reports whether this authority may sign for a host - the same question Leaf answers by
// refusing, asked before a binding is accepted rather than when a request arrives.
//
// A destroyed authority covers nothing: it can sign nothing.
func (c *CA) Covers(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.destroyed {
		return false
	}

	return coveredBy(c.hosts, host)
}

func (c *CA) Leaf(host string) (*tls.Certificate, error) {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.destroyed {
		return nil, ErrDestroyed
	}

	if got, ok := c.leaves[host]; ok {
		return got, nil
	}

	if !coveredBy(c.hosts, host) {
		return nil, invalid("this sandbox's credential authority may not sign for %q: it is "+
			"constrained to %s. Add a binding for that host, or send the request to a host a "+
			"binding already names", host, strings.Join(c.hosts, ", "))
	}

	serial, err := serialNumber()
	if err != nil {
		return nil, err
	}

	now := time.Now()
	notAfter := now.Add(leafLifetime)

	// Never past the authority: a leaf valid after its issuer is a leaf that fails verification
	// for a reason the message will not mention.
	if notAfter.After(c.cert.NotAfter) {
		notAfter = c.cert.NotAfter
	}

	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: host},
		DNSNames:              []string{host},
		NotBefore:             now.Add(-backdate),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &c.leafKey.PublicKey, c.key)
	if err != nil {
		return nil, fmt.Errorf("minting a certificate for %s: %w", host, err)
	}

	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("reading back the certificate for %s: %w", host, err)
	}

	got := &tls.Certificate{
		Certificate: [][]byte{der, c.der},
		PrivateKey:  c.leafKey,
		Leaf:        leaf,
	}
	c.leaves[host] = got

	return got, nil
}

// Destroy drops the key and every leaf. Called when the sandbox's filter goes, so the authority
// stops existing with the sandbox it was minted for.
//
// The key's scalar is zeroed as well as dropped. That is not a guarantee - a big.Int has held
// other allocations and Go does not promise to overwrite them - but a zeroed scalar is one fewer
// copy in a heap dump, and the certificate it signed for is useless without it.
func (c *CA) Destroy() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.destroyed = true

	for _, k := range []*ecdsa.PrivateKey{c.key, c.leafKey} {
		if k != nil && k.D != nil {
			k.D.SetInt64(0)
		}
	}

	c.key, c.leafKey = nil, nil
	c.leaves = map[string]*tls.Certificate{}
}

// dnsConstraints turns binding hosts into RFC 5280 DNS name constraints.
//
// "*.example.com" becomes ".example.com", which permits subdomains and not the apex - exactly what
// the wildcard means. A bare "example.com" becomes "example.com", which by this extension's
// suffix rule also permits its subdomains. That is wider than the binding, and there is no
// narrower form: the extension has no "this name only" spelling. It does not widen what gets
// intercepted, because that is decided by the binding match on every request; the constraint is
// the ceiling on what the key could ever sign, not the rule for what it does sign.
func dnsConstraints(hosts []string) ([]string, error) {
	if len(hosts) == 0 {
		return nil, invalid("a credential authority needs at least one host to be constrained to")
	}

	seen := map[string]bool{}

	var out []string

	for _, h := range hosts {
		h = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")

		switch {
		case h == "":
			return nil, invalid("a credential authority cannot be constrained to an empty host")
		case strings.HasPrefix(h, "*."):
			h = h[1:]
		}

		// An address has no place in a DNS constraint, and the IP constraint is excluded
		// outright, so a binding on one could never be served. Refused where it is written.
		if ip := net.ParseIP(strings.TrimPrefix(h, ".")); ip != nil {
			return nil, invalid("credential binding host %q is an address: a binding names an "+
				"FQDN, because the certificate the sandbox verifies is issued for a name", h)
		}

		if !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}

	return out, nil
}

// coveredBy reports whether host falls inside the constraint list, by the same suffix rule a
// verifier applies: ".example.com" covers a subdomain, "example.com" covers itself and its
// subdomains.
func coveredBy(constraints []string, host string) bool {
	if host == "" {
		return false
	}

	for _, c := range constraints {
		switch {
		case strings.HasPrefix(c, "."):
			if strings.HasSuffix(host, c) && len(host) > len(c) {
				return true
			}
		case host == c, strings.HasSuffix(host, "."+c):
			return true
		}
	}

	return false
}

// pemBlock wraps DER in the PEM armour a trust store reads.
func pemBlock(kind string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der})
}

// everyIPRange is both address families in full, for ExcludedIPRanges.
func everyIPRange() []*net.IPNet {
	return []*net.IPNet{
		{IP: net.IPv4zero.To4(), Mask: net.CIDRMask(0, 32)},
		{IP: net.IPv6zero, Mask: net.CIDRMask(0, 128)},
	}
}

// serialNumber is a random 128-bit serial, as a public CA would use: a predictable serial is one
// fewer unknown for anybody trying to forge a sibling certificate.
func serialNumber() (*big.Int, error) {
	max := new(big.Int).Lsh(big.NewInt(1), 128)

	n, err := rand.Int(rand.Reader, max)
	if err != nil {
		return nil, fmt.Errorf("choosing a certificate serial number: %w", err)
	}

	return n.Add(n, big.NewInt(1)), nil
}
