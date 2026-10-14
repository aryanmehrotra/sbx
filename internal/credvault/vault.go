package credvault

// One vault per sandbox, and a registry of them.
//
// A vault exists only where the daemon hosts the sandbox's filter itself. That is not a check
// somewhere, it is where vaults are made: daemon/egressproxy.go opens one beside the filter it
// binds on the sandbox's bridge gateway, and closes it when that filter goes. On a host where the
// daemon cannot bind that gateway - a VM-backed docker, which is every Mac - the filter runs as a
// container instead, no vault is ever opened, and every call on the Credential Vault API is
// refused by name.
//
// That arrangement is the point rather than an implementation detail. A vault in the container
// filter would hold the certificate authority's private key and every secret inside a container on
// the sandbox's own bridge, configured over an endpoint with no redaction, and one secret in a log
// is a secret in somebody else's hands. Better to say the host cannot do this than to do it
// weakly: the create is refused, with the reason and the fix.

import (
	"sort"
	"strings"
	"sync"

	"github.com/aryanmehrotra/sbx/internal/egress"
)

// Registry is the vaults this daemon holds. One entry per sandbox whose filter it hosts.
//
// Nothing here is written to disk, so a vault does not survive `sbx serve` restarting. That is the
// same trade the certificate authority makes, and it is the safe direction: a key that survives a
// restart is a key on a filesystem. A caller whose daemon restarted gets 404 from the vault routes
// and sends its POST again.
type Registry struct {
	mu     sync.Mutex
	vaults map[string]*Vault
}

// NewRegistry makes an empty registry.
func NewRegistry() *Registry { return &Registry{vaults: map[string]*Vault{}} }

// Open returns the sandbox's vault, making an empty one if it has none.
//
// policy reports the egress policy in force and whether the sandbox has a filter at all; the vault
// asks it on every write, because a binding on a host the policy denies is a credential that can
// never be sent, and the policy can change under a vault that already exists.
func (r *Registry) Open(sandbox string, policy func() (egress.Policy, bool)) *Vault {
	r.mu.Lock()
	defer r.mu.Unlock()

	if v, ok := r.vaults[sandbox]; ok {
		return v
	}

	v := &Vault{sandbox: sandbox, policy: policy,
		credentials: map[string]record{}, bindings: map[string]Binding{}}
	r.vaults[sandbox] = v

	return v
}

// Lookup is the sandbox's vault, or false when this daemon holds none - which is the answer the
// API's routes turn into a refusal naming the host.
func (r *Registry) Lookup(sandbox string) (*Vault, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	v, ok := r.vaults[sandbox]

	return v, ok
}

// Close drops the sandbox's vault and destroys its certificate authority. Called when the filter
// it belongs to goes, so the key stops existing with the sandbox.
func (r *Registry) Close(sandbox string) {
	r.mu.Lock()
	v, ok := r.vaults[sandbox]
	delete(r.vaults, sandbox)
	r.mu.Unlock()

	if ok {
		v.destroy()
	}
}

// Vault is one sandbox's credentials and bindings, with the authority that lets the filter read
// the requests they apply to.
//
// Every method is safe for concurrent use: the API's routes write it while the data path reads it.
type Vault struct {
	sandbox string
	policy  func() (egress.Policy, bool)

	mu          sync.RWMutex
	exists      bool
	revision    int64
	credentials map[string]record
	bindings    map[string]Binding
	ca          *CA
}

// Sandbox is the sandbox this vault belongs to.
func (v *Vault) Sandbox() string { return v.sandbox }

// Create makes the first revision. A vault that already exists is a conflict, not a replacement:
// POST twice is a client that lost track of what it configured, and silently discarding the first
// set of bindings is the wrong guess.
func (v *Vault) Create(req CreateRequest) (State, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	if v.exists {
		return State{}, ErrExists
	}

	credentials := map[string]record{}
	bindings := map[string]Binding{}

	for _, c := range req.Credentials {
		rec, err := normalizeCredential(c, 1)
		if err != nil {
			return State{}, err
		}

		if _, dup := credentials[rec.Name]; dup {
			return State{}, invalid("credentials names %q twice", rec.Name)
		}

		credentials[rec.Name] = rec
	}

	for _, b := range req.Bindings {
		got, err := normalizeBinding(b)
		if err != nil {
			return State{}, err
		}

		if _, dup := bindings[got.Name]; dup {
			return State{}, invalid("bindings names %q twice", got.Name)
		}

		bindings[got.Name] = got
	}

	if err := v.check(credentials, bindings); err != nil {
		return State{}, err
	}

	if err := v.remintCA(bindings); err != nil {
		return State{}, err
	}

	v.exists, v.revision, v.credentials, v.bindings = true, 1, credentials, bindings

	return v.stateLocked(), nil
}

// Patch applies add, replace and delete to credentials and bindings, whole or not at all.
//
// Whole or not at all because half of a credential change is a sandbox authenticating to one host
// and not another, with nothing in the response to say which. The candidate is built as a copy,
// validated, and only then swapped in.
func (v *Vault) Patch(req MutationRequest) (State, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	if !v.exists {
		return State{}, ErrNotFound
	}

	if req.ExpectedRevision != nil && *req.ExpectedRevision != v.revision {
		return State{}, &RevisionError{Expected: *req.ExpectedRevision, Current: v.revision}
	}

	next := v.revision + 1

	credentials := map[string]record{}
	for k, c := range v.credentials {
		credentials[k] = c
	}

	bindings := map[string]Binding{}
	for k, b := range v.bindings {
		bindings[k] = b
	}

	if err := applyCredentials(credentials, req.Credentials, next); err != nil {
		return State{}, err
	}

	if err := applyBindings(bindings, req.Bindings); err != nil {
		return State{}, err
	}

	if err := v.check(credentials, bindings); err != nil {
		return State{}, err
	}

	if err := v.remintCA(bindings); err != nil {
		return State{}, err
	}

	v.revision, v.credentials, v.bindings = next, credentials, bindings

	return v.stateLocked(), nil
}

// Delete removes the vault and destroys its authority. The sandbox keeps running; it just has no
// credentials of its own any more.
func (v *Vault) Delete() error {
	v.mu.Lock()
	defer v.mu.Unlock()

	if !v.exists {
		return ErrNotFound
	}

	v.exists, v.revision = false, 0
	v.credentials, v.bindings = map[string]record{}, map[string]Binding{}

	if v.ca != nil {
		v.ca.Destroy()
		v.ca = nil
	}

	return nil
}

// State is the sanitized vault. Credential values are never in it: the API is write-only for
// plaintext, and this type has no field that could carry one.
func (v *Vault) State() (State, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()

	if !v.exists {
		return State{}, ErrNotFound
	}

	return v.stateLocked(), nil
}

// CredentialList is the credential metadata alone.
func (v *Vault) CredentialList() (CredentialList, error) {
	st, err := v.State()
	if err != nil {
		return CredentialList{}, err
	}

	return CredentialList{Revision: st.Revision, Credentials: st.Credentials}, nil
}

// Credential is one credential's metadata.
func (v *Vault) Credential(name string) (Metadata, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()

	if !v.exists {
		return Metadata{}, ErrNotFound
	}

	c, ok := v.credentials[name]
	if !ok {
		return Metadata{}, invalid("this vault has no credential %q", name)
	}

	return Metadata{Name: c.Name, SourceType: c.SourceType, Revision: c.Revision}, nil
}

// BindingList is the binding metadata alone.
func (v *Vault) BindingList() (BindingList, error) {
	st, err := v.State()
	if err != nil {
		return BindingList{}, err
	}

	return BindingList{Revision: st.Revision, Bindings: st.Bindings}, nil
}

// Binding is one binding's metadata.
func (v *Vault) Binding(name string) (BindingMetadata, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()

	if !v.exists {
		return BindingMetadata{}, ErrNotFound
	}

	b, ok := v.bindings[name]
	if !ok {
		return BindingMetadata{}, invalid("this vault has no binding %q", name)
	}

	return bindingMetadata(b, v.revision), nil
}

// CA is the authority for the revision in force, or nil when the vault holds no binding and so has
// nothing to sign for.
func (v *Vault) CA() *CA {
	v.mu.RLock()
	defer v.mu.RUnlock()

	return v.ca
}

// Bound implements egress.CredentialSource: whether a binding covers this destination.
//
// The port decides the scheme rather than the other way round, because that is all a CONNECT
// carries: 443 is an https binding's port and 80 an http one's, and any other port is not a
// binding's at all - the vault adds credentials to HTTP requests, and a filter that guessed on
// port 8443 would be terminating TLS on a protocol it cannot read.
func (v *Vault) Bound(host, port string) bool {
	scheme := ""

	switch port {
	case "443":
		scheme = "https"
	case "80":
		scheme = "http"
	default:
		return false
	}

	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")

	v.mu.RLock()
	defer v.mu.RUnlock()

	if !v.exists {
		return false
	}

	for _, b := range v.bindings {
		if contains(b.Match.Schemes, scheme) && matchesHost(b.Match.Hosts, host) {
			return true
		}
	}

	return false
}

// destroy is Registry.Close's half: the authority goes, and the vault reports itself gone.
func (v *Vault) destroy() {
	v.mu.Lock()
	defer v.mu.Unlock()

	v.exists, v.revision = false, 0
	v.credentials, v.bindings = map[string]record{}, map[string]Binding{}

	if v.ca != nil {
		v.ca.Destroy()
		v.ca = nil
	}
}

// check validates a candidate vault against the policy in force.
func (v *Vault) check(credentials map[string]record, bindings map[string]Binding) error {
	pol, hasPolicy := egress.Policy{}, false
	if v.policy != nil {
		pol, hasPolicy = v.policy()
	}

	return validateCandidate(credentials, bindings, pol, hasPolicy)
}

// remintCA gives the vault an authority constrained to exactly the hosts its bindings name.
//
// It mints a new one only when that host set changes, and not on every revision. A new authority
// is a new certificate for the sandbox's trust store, so re-minting when nothing about the hosts
// moved would break every open client to rotate a secret - and rotating a secret without touching
// the hosts is the common case.
func (v *Vault) remintCA(bindings map[string]Binding) error {
	hosts := boundHosts(bindings)

	if len(hosts) == 0 {
		if v.ca != nil {
			v.ca.Destroy()
			v.ca = nil
		}

		return nil
	}

	if v.ca != nil && sameHosts(v.ca.Hosts(), dnsFor(hosts)) {
		return nil
	}

	ca, err := NewCA(hosts)
	if err != nil {
		return err
	}

	if v.ca != nil {
		v.ca.Destroy()
	}

	v.ca = ca

	return nil
}

// boundHosts is every host the bindings name, sorted and deduplicated.
func boundHosts(bindings map[string]Binding) []string {
	seen := map[string]bool{}

	var out []string

	for _, b := range bindings {
		for _, h := range b.Match.Hosts {
			if !seen[h] {
				seen[h] = true
				out = append(out, h)
			}
		}
	}

	sort.Strings(out)

	return out
}

// dnsFor is what NewCA would make of these hosts, so a host set can be compared with an existing
// authority's constraints without minting one to find out.
func dnsFor(hosts []string) []string {
	got, err := dnsConstraints(hosts)
	if err != nil {
		return nil
	}

	return got
}

func sameHosts(a, b []string) bool {
	if len(a) != len(b) || len(a) == 0 {
		return false
	}

	x, y := append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)

	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}

	return true
}

// stateLocked is the sanitized state. Callers hold the lock.
func (v *Vault) stateLocked() State {
	st := State{
		Revision:    v.revision,
		Credentials: make([]Metadata, 0, len(v.credentials)),
		Bindings:    make([]BindingMetadata, 0, len(v.bindings)),
	}

	for _, c := range v.credentials {
		st.Credentials = append(st.Credentials,
			Metadata{Name: c.Name, SourceType: c.SourceType, Revision: c.Revision})
	}

	for _, b := range v.bindings {
		st.Bindings = append(st.Bindings, bindingMetadata(b, v.revision))
	}

	// Sorted by name: a map's order is not an order, and a caller that diffs two GETs would see
	// a change on every call.
	sort.Slice(st.Credentials, func(i, j int) bool { return st.Credentials[i].Name < st.Credentials[j].Name })
	sort.Slice(st.Bindings, func(i, j int) bool { return st.Bindings[i].Name < st.Bindings[j].Name })

	return st
}

func bindingMetadata(b Binding, revision int64) BindingMetadata {
	match := b.Match

	return BindingMetadata{Name: b.Name, Revision: revision, Match: &match, Auth: sanitizeAuth(b.Auth)}
}

// matchesHost reports whether host is covered by a binding's host list: an exact name, or a
// "*."-wildcard over a strict subdomain.
func matchesHost(hosts []string, host string) bool {
	for _, h := range hosts {
		switch {
		case strings.HasPrefix(h, "*."):
			if suffix := h[1:]; strings.HasSuffix(host, suffix) && len(host) > len(suffix) {
				return true
			}
		case h == host:
			return true
		}
	}

	return false
}

// applyCredentials applies one mutation set. Delete first, then replace, then add, so a single
// PATCH can retire a name and reuse it - and a name touched by two operations is refused, because
// which one wins would depend on this order rather than on what the caller asked for.
func applyCredentials(credentials map[string]record, set *CredentialMutations, revision int64) error {
	if set == nil {
		return nil
	}

	touched := map[string]bool{}

	for _, name := range set.Delete {
		name = strings.TrimSpace(name)
		if name == "" {
			return invalid("credentials.delete has a blank name")
		}

		if touched[name] {
			return invalid("credentials mentions %q in more than one operation", name)
		}

		touched[name] = true

		if _, ok := credentials[name]; !ok {
			return invalid("credentials.delete names %q, which this vault does not have", name)
		}

		delete(credentials, name)
	}

	for _, raw := range set.Replace {
		rec, err := normalizeCredential(raw, revision)
		if err != nil {
			return err
		}

		if touched[rec.Name] {
			return invalid("credentials mentions %q in more than one operation", rec.Name)
		}

		touched[rec.Name] = true

		if _, ok := credentials[rec.Name]; !ok {
			return invalid("credentials.replace names %q, which this vault does not have - add it "+
				"with credentials.add", rec.Name)
		}

		credentials[rec.Name] = rec
	}

	for _, raw := range set.Add {
		rec, err := normalizeCredential(raw, revision)
		if err != nil {
			return err
		}

		if touched[rec.Name] {
			return invalid("credentials mentions %q in more than one operation", rec.Name)
		}

		touched[rec.Name] = true

		if _, ok := credentials[rec.Name]; ok {
			return invalid("credentials.add names %q, which this vault already has - change it "+
				"with credentials.replace", rec.Name)
		}

		credentials[rec.Name] = rec
	}

	return nil
}

// applyBindings is applyCredentials for bindings.
func applyBindings(bindings map[string]Binding, set *BindingMutations) error {
	if set == nil {
		return nil
	}

	touched := map[string]bool{}

	for _, name := range set.Delete {
		name = strings.TrimSpace(name)
		if name == "" {
			return invalid("bindings.delete has a blank name")
		}

		if touched[name] {
			return invalid("bindings mentions %q in more than one operation", name)
		}

		touched[name] = true

		if _, ok := bindings[name]; !ok {
			return invalid("bindings.delete names %q, which this vault does not have", name)
		}

		delete(bindings, name)
	}

	for _, raw := range set.Replace {
		b, err := normalizeBinding(raw)
		if err != nil {
			return err
		}

		if touched[b.Name] {
			return invalid("bindings mentions %q in more than one operation", b.Name)
		}

		touched[b.Name] = true

		if _, ok := bindings[b.Name]; !ok {
			return invalid("bindings.replace names %q, which this vault does not have - add it "+
				"with bindings.add", b.Name)
		}

		bindings[b.Name] = b
	}

	for _, raw := range set.Add {
		b, err := normalizeBinding(raw)
		if err != nil {
			return err
		}

		if touched[b.Name] {
			return invalid("bindings mentions %q in more than one operation", b.Name)
		}

		touched[b.Name] = true

		if _, ok := bindings[b.Name]; ok {
			return invalid("bindings.add names %q, which this vault already has - change it with "+
				"bindings.replace", b.Name)
		}

		bindings[b.Name] = b
	}

	return nil
}
