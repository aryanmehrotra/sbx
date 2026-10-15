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
	"net"
	"sort"
	"strings"
	"sync"

	"github.com/aryanmehrotra/sbx/internal/egress"
)

// Registry is the vaults this daemon holds. One entry per sandbox whose filter it hosts.
//
// Nothing here is written to disk, so a vault does not survive `sbx serve` restarting. That is the
// same trade the certificate authority makes, and it is the safe direction: a key that survives a
// restart is a key on a filesystem.
//
// What a restart loses is not the vault's contents, which were never promised to survive one. It
// is the only authority the sandbox will accept: the certificate was mounted into its trust store
// when it was created and is read once by whatever runs in there. So a sandbox whose daemon
// restarted cannot have a vault again at all, and is told that (ErrAuthorityLost) rather than
// given one that signs with an authority nothing in there has ever seen.
type Registry struct {
	mu     sync.Mutex
	vaults map[string]*Vault

	// cas are authorities minted before their vault exists, by Certificate, so that a sandbox's
	// trust store can be delivered when the sandbox is created. Open adopts one rather than
	// minting its own, because the certificate already in the sandbox has to be the one the
	// filter signs with.
	cas map[string]*CA
}

// NewRegistry makes an empty registry.
func NewRegistry() *Registry {
	return &Registry{vaults: map[string]*Vault{}, cas: map[string]*CA{}}
}

// Certificate is the PEM of the authority this sandbox's filter will sign with, minted now if it
// does not exist yet.
//
// It is called while the sandbox is being created, before any filter or vault does, because the
// certificate has to be in the image's trust store from the first process that reads it - and that
// is read once, at startup, by Node, Python and Go alike. The authority is constrained to the
// hosts the policy allows, which a binding can never exceed, so the same certificate stays correct
// for every binding the sandbox goes on to make.
//
// An empty PEM and no error is a policy with no names of its own: there is nothing to terminate
// TLS for, so there is nothing to trust.
//
// This is the only place an authority is minted. The constraints come from the POLICY and not from
// the bindings, which is what makes it stable: the trust store is read once by whatever runs in
// there - Node, Python and Go all cache it at startup - so an authority that changed when a
// binding was added would leave a long-running agent trusting one that no longer exists, and every
// swap after that would fail its handshake with nothing to say why. Minting from the binding set
// did exactly that.
//
// It is not wider than it needs to be either, because a binding cannot name a host the policy does
// not already allow - validateCandidate refuses that with pol.AllowsName, at write time, every
// time. So the policy's hosts are a superset of the bindings' hosts by construction: these
// constraints cover every leaf the vault could ever be asked to mint, and never a host the sandbox
// is not allowed to reach anyway. The exclusion of every IP range is unchanged - an authority that
// could certify an address rather than a name would be a general-purpose one.
func (r *Registry) Certificate(sandbox string, pol egress.Policy) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if v, ok := r.vaults[sandbox]; ok {
		if ca := v.CA(); ca != nil {
			return ca.PEM(), nil
		}
	}

	if ca, ok := r.cas[sandbox]; ok {
		return ca.PEM(), nil
	}

	hosts := hostsOf(pol)
	if len(hosts) == 0 {
		return nil, nil
	}

	ca, err := NewCA(hosts)
	if err != nil {
		return nil, err
	}

	r.cas[sandbox] = ca

	return ca.PEM(), nil
}

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

	// The authority the sandbox was already given, when its trust store was delivered at create -
	// and nothing else. A vault never mints its own: the certificate the sandbox trusts is read
	// once, at startup, by Node, Python and Go alike, so a second authority here would be one the
	// sandbox has never seen and every bound host would fail its handshake with `unknown
	// authority`, after a write that answered 201.
	switch ca, ok := r.cas[sandbox]; {
	case ok:
		v.ca = ca

		delete(r.cas, sandbox)

	case len(v.policyHosts()) > 0:
		// The policy names hosts, so this sandbox was given an authority when it was created and
		// this registry is not the one that minted it - `sbx serve` was restarted since. There is
		// nothing to recover, because the key was only ever in that process's memory, so the vault
		// is marked unusable here and every write on it says so.
		v.noAuthority = ErrAuthorityLost
	}

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

	// A certificate minted for a sandbox whose filter never came up, so its key does not outlive
	// the sandbox either.
	if ca, pre := r.cas[sandbox]; pre {
		ca.Destroy()
		delete(r.cas, sandbox)
	}

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

	// noAuthority is why this vault can never sign for anything, set once by Open and read-only
	// after it. Nil is the ordinary case.
	noAuthority error
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

	v.revision, v.credentials, v.bindings = next, credentials, bindings

	return v.stateLocked(), nil
}

// Delete removes the vault's credentials and bindings. The sandbox keeps running; it just has no
// credentials of its own any more.
//
// The authority is left alone, because it belongs to the sandbox rather than to this revision: its
// certificate is in the sandbox's trust store, read once at startup, and nothing can replace it
// there while the sandbox runs. Destroying the key here would make the next POST a vault signing
// with an authority the sandbox has never seen - or, now that nothing re-mints, no vault at all.
// The key still stops existing with the sandbox: Registry.Close destroys it when the filter goes.
func (v *Vault) Delete() error {
	v.mu.Lock()
	defer v.mu.Unlock()

	if !v.exists {
		return ErrNotFound
	}

	v.exists, v.revision = false, 0
	v.credentials, v.bindings = map[string]record{}, map[string]Binding{}

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

// Authority is why this vault can never sign for anything, or nil.
//
// The API asks before it answers a call on a sandbox whose authority this daemon lost, so the
// caller gets the reason and the one thing that fixes it rather than an empty 404.
func (v *Vault) Authority() error {
	v.mu.RLock()
	defer v.mu.RUnlock()

	return v.noAuthority
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
	// Before the request is looked at, because no request can be honoured: the sandbox trusts an
	// authority this daemon does not hold, so a credential written here could never be sent.
	if v.noAuthority != nil {
		return v.noAuthority
	}

	pol, hasPolicy := egress.Policy{}, false
	if v.policy != nil {
		pol, hasPolicy = v.policy()
	}

	if err := validateCandidate(credentials, bindings, pol, hasPolicy); err != nil {
		return err
	}

	return v.certifiable(bindings)
}

// certifiable refuses a binding whose host this sandbox's authority may not sign for.
//
// The authority is minted once, when the sandbox is created, from the policy as it stood then -
// that is what makes the certificate in the sandbox's trust store stay true. A policy widened
// afterwards does not widen it, so a host allowed later and then bound would be accepted here and
// fail at the first request, when Leaf refuses to sign: a 502 on a tunnel, far from the PATCH that
// caused it. Refused at the write instead, which is where the caller can still do something about
// it.
func (v *Vault) certifiable(bindings map[string]Binding) error {
	if len(bindings) == 0 {
		return nil
	}

	// No authority at all, which is a policy that named no host when the sandbox was created: the
	// trust store got nothing, so nothing can be signed for. Widening the policy afterwards does
	// not change that - the certificate in there is read once - so this is refused at the write
	// too, for the same reason a host added later is.
	if v.ca == nil {
		return invalid("this sandbox has no credential authority, so no binding on it could ever " +
			"be signed for: its egress policy named no host when the sandbox was created, so " +
			"there was nothing to put in its trust store. Create the sandbox with the host the " +
			"binding names already in its networkPolicy")
	}

	for _, b := range bindings {
		for _, host := range b.Match.Hosts {
			// A wildcard is tested through a name under it, the way the policy check tests one.
			probe := host
			if strings.HasPrefix(host, "*.") {
				probe = "probe" + host[1:]
			}

			if v.ca.Covers(probe) {
				continue
			}

			return invalid("binding %q names the host %q, which this sandbox's credential "+
				"authority may not sign for: it was minted when the sandbox was created, from "+
				"the egress policy as it stood then, and it is constrained to %s. That is "+
				"deliberate - the certificate in the sandbox's trust store is read once, so the "+
				"authority cannot be replaced under a running agent. Create the sandbox with "+
				"%q already in its networkPolicy to bind it",
				b.Name, host, strings.Join(v.ca.Hosts(), ", "), host)
		}
	}

	return nil
}

// policyHosts is every DNS name the sandbox's egress policy allows, which is what the authority is
// constrained to. Callers must hold v.mu, or call before the vault is shared.
//
// An address or a CIDR target is skipped rather than refused: a policy may hold both, and only the
// names can ever be the subject of a leaf.
func (v *Vault) policyHosts() []string {
	if v.policy == nil {
		return nil
	}

	pol, ok := v.policy()
	if !ok {
		return nil
	}

	return hostsOf(pol)
}

// hostsOf is every DNS name a policy allows, sorted and deduplicated.
func hostsOf(pol egress.Policy) []string {
	seen := map[string]bool{}

	var out []string

	for _, rule := range pol.Egress {
		if !strings.EqualFold(rule.Action, "allow") {
			continue
		}

		host := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(rule.Target)), ".")
		if host == "" || seen[host] || !isDNSTarget(host) {
			continue
		}

		seen[host] = true
		out = append(out, host)
	}

	sort.Strings(out)

	return out
}

// isDNSTarget reports whether a policy target is a name rather than an address or a CIDR.
func isDNSTarget(target string) bool {
	if strings.Contains(target, "/") {
		return false
	}

	probe := strings.TrimPrefix(target, "*.")

	return net.ParseIP(probe) == nil
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
