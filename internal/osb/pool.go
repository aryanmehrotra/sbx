package osb

// The warm pool: sandboxes created before anyone asks, so a create can be answered in
// milliseconds instead of the hundreds a container start costs.
//
// ComputeSDK's Burst TTI - create() to the first command, a hundred at once - is the benchmark
// this exists for, and every sandbox in it is new: wake-on-connect cannot help, and `docker run`
// alone is 150 ms on a quiet engine. What can is a container that already exists, execd already
// answering, frozen so it costs no CPU while it waits.
//
// A member is an ordinary API sandbox whose record carries its pool key and nothing a caller
// gave. That is what keeps it invisible - GET, list, delete and diagnostics all treat a pooled
// record as absent - while everything underneath (provisioning, the daemon fronting it, removal)
// is the same code a normal create runs. A claim then gives it a caller: the record takes the
// request's metadata, expiry and a fresh token, and execd is re-keyed and handed the request's
// env through POST /sbx/claim. The id was minted when the member was made and never shown to
// anyone, so it is as new to the caller as a cold create's.
//
// What a member cannot take on is anything that shapes the container: image, entrypoint, limits,
// ports, platform, idle mode, a network policy. A request that differs in any of them is not
// served from the pool; it takes the cold path, which is the same create it always was.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aryanmehrotra/sbx/internal/history"
	"github.com/aryanmehrotra/sbx/internal/logs"
)

// poolEntrypoint and poolLimits are what the OpenSandbox SDKs send when the caller leaves them
// out (DefaultEntrypoint and DefaultResourceLimits in sdks/sandbox/go/constants.go at
// release-1.1.0), so a pool built from them serves the SDKs' plain create(image).
var (
	poolEntrypoint = []string{"tail", "-f", "/dev/null"}
	poolLimits     = map[string]string{"cpu": "1", "memory": "2Gi"}
)

// PoolSpec is one --osb-pool entry: keep Size sandboxes of Image ready.
type PoolSpec struct {
	Image string
	Size  int
}

// defaultPoolSize is what IMAGE alone means.
const defaultPoolSize = 8

// ParsePool reads IMAGE[=N]. An image reference can contain ':' and '@' but never '=', so the
// last '=' is unambiguous.
func ParsePool(v string) (PoolSpec, error) {
	v = strings.TrimSpace(v)

	img, n, hasN := v, "", false
	if i := strings.LastIndex(v, "="); i >= 0 {
		img, n, hasN = v[:i], v[i+1:], true
	}

	img = strings.TrimSpace(img)
	if img == "" {
		return PoolSpec{}, fmt.Errorf("--osb-pool %q names no image - for example node:22-slim=10", v)
	}

	size := defaultPoolSize

	if hasN {
		var err error

		size, err = strconv.Atoi(strings.TrimSpace(n))
		if err != nil || size < 1 {
			return PoolSpec{}, fmt.Errorf("--osb-pool %q: the size after = must be a whole number of "+
				"sandboxes, at least 1 - for example %s=10", v, img)
		}
	}

	return PoolSpec{Image: img, Size: size}, nil
}

// pool is the ready members of one key and the bookkeeping to keep it full.
type pool struct {
	spec     PoolSpec
	key      string
	template createRequest

	mu      sync.Mutex
	ready   []poolMember
	filling int

	// failures counts members that did not make it in a row; it spaces the retries, so an image
	// that cannot be pulled is not a create loop running flat out for the life of the daemon.
	failures int

	wake chan struct{}
}

// poolMember is a frozen sandbox ready to be claimed.
type poolMember struct {
	id    string
	token string // the token it was started with; only this process ever held it
	addr  string // execd through the daemon's wake port
}

func (p *pool) poke() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// take removes one ready member, or reports there is none.
func (p *pool) take() (poolMember, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.ready) == 0 {
		return poolMember{}, false
	}

	m := p.ready[0]
	p.ready = p.ready[1:]

	return m, true
}

// poolKey is everything that shapes the container of a validated plan. Two plans with the same
// key get containers no caller could tell apart, so a member made for one can serve the other.
// Empty means the plan cannot be served from any pool.
func poolKey(pl plan) string {
	if pl.egressPolicy != nil {
		return ""
	}

	plat := ""
	if p := pl.rec.Platform; p != nil {
		plat = p.OS + "/" + p.Arch
	}

	ports := make([]string, 0, len(pl.rec.Ports))
	for _, p := range pl.rec.Ports {
		ports = append(ports, strconv.Itoa(p))
	}

	return strings.Join([]string{
		pl.rec.Image,
		strings.Join(pl.rec.Entrypoint, "\x00"),
		pl.cpu, pl.memory, pl.gpus, pl.onIdle,
		strings.Join(ports, ","),
		plat,
	}, "\x01")
}

// newPools builds one pool per spec. A spec that does not validate is a startup error: a pool
// that can never fill is a misconfiguration to say at once, not a log line every few seconds.
func (s *Server) newPools(specs []PoolSpec) error {
	for _, sp := range specs {
		tpl := createRequest{
			Image:          &imageSpec{URI: sp.Image},
			Entrypoint:     slices.Clone(poolEntrypoint),
			ResourceLimits: poolLimits,
		}

		pl, status, _, msg := s.validate(tpl)
		if status != 0 {
			return fmt.Errorf("--osb-pool %s: %s", sp.Image, msg)
		}

		key := poolKey(pl)

		if old, ok := s.pools[key]; ok {
			old.spec.Size += sp.Size
			continue
		}

		s.pools[key] = &pool{spec: sp, key: key, template: tpl, wake: make(chan struct{}, 1)}
	}

	return nil
}

// fromPool answers a create from a warm member when the plan matches a pool. It reports false,
// having written nothing, when there is no pool for the plan or no member ready - the caller
// then takes the cold path.
func (s *Server) fromPool(w http.ResponseWriter, r *http.Request, req createRequest, pl plan) bool {
	if req.Extensions["sbx.pool"] == "off" {
		return false
	}

	p := s.pools[poolKey(pl)]
	if p == nil {
		return false
	}

	// Refilled whatever happens next: a member taken is a member to replace, and one that
	// turns out to be broken is too.
	defer p.poke()

	for {
		m, ok := p.take()
		if !ok {
			return false
		}

		rec, err := s.claim(r.Context(), m, pl)
		if err != nil {
			logs.Default.Warn(m.id, service, "osb: pool member could not be claimed, discarding it: %v", err)
			s.discard(m.id)

			continue
		}

		resp := render(rec, nil, false)
		at := rec.LastTransitionAt
		resp.Status = statusJSON{State: stateRunning, LastTransitionAt: &at}

		s.trace.mark(rec.ID, "create answered Running (warm pool)")

		w.Header().Set("Location", "/v1/sandboxes/"+rec.ID)
		writeJSON(w, http.StatusAccepted, resp)

		return true
	}
}

// claim hands a member to the caller of pl: thawed, execd re-keyed with pl's token and given
// pl's env, and the record rewritten as the caller's sandbox.
func (s *Server) claim(ctx context.Context, m poolMember, pl plan) (record, error) {
	s.trace.begin(m.id)

	if err := s.rt.Thaw(ctx, m.id); err != nil {
		return record{}, fmt.Errorf("thawing: %w", err)
	}

	s.trace.mark(m.id, "thawed")

	if err := s.claimExecd(ctx, m.addr, m.token, pl.rec.Token, pl.env); err != nil {
		return record{}, fmt.Errorf("re-keying execd: %w", err)
	}

	s.trace.mark(m.id, "execd re-keyed")

	now := s.now().UTC()

	s.mu.Lock()

	r, ok := s.recs[m.id]
	if !ok || r.Pool == "" {
		s.mu.Unlock()
		return record{}, errors.New("the member's record is gone")
	}

	r.Pool = ""
	r.Metadata = pl.rec.Metadata
	r.Extensions = pl.rec.Extensions
	r.Token = pl.rec.Token
	r.CreatedAt = now
	r.ExpiresAt = pl.rec.ExpiresAt
	r.transition(stateRunning, "", "", now)

	if err := s.store.save(r); err != nil {
		// Not claimed, then: a sandbox whose expiry and token live only in memory would come
		// back after a restart as a pool member, and be removed with the caller's work in it.
		r.Pool = "reclaim-failed"
		s.mu.Unlock()

		return record{}, fmt.Errorf("recording the claim under %s: %w", s.store.dir, err)
	}

	s.mu.Unlock()

	history.Append(history.Record{Kind: "event", Sandbox: m.id, Event: "created", Actor: "osb",
		Message: "image " + pl.rec.Image + " (from the warm pool)"})

	rec, _ := s.snapshot(m.id)

	return rec, nil
}

// claimExecdHTTP is the real Options.Claim: execd's POST /sbx/claim, through the wake port.
func claimExecdHTTP(ctx context.Context, addr, oldToken, newToken string, env map[string]string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	body, _ := json.Marshal(map[string]any{"accessToken": newToken, "envs": env})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+"/sbx/claim", bytes.NewReader(body))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(tokenHeader, oldToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		var e errorBody
		_ = json.NewDecoder(resp.Body).Decode(&e)

		return fmt.Errorf("execd answered %s: %s %s - is the execd volume from an sbx older than "+
			"the warm pool? `docker volume rm` it and restart sbx serve", resp.Status, e.Code, e.Message)
	}

	return nil
}

// discard removes a member that cannot be used, off the request path.
func (s *Server) discard(id string) {
	s.wg.Add(1)

	go func() {
		defer s.wg.Done()

		ctx, cancel := context.WithTimeout(context.WithoutCancel(s.base), time.Minute)
		defer cancel()

		if err := s.remove(ctx, id, "pool"); err != nil {
			logs.Default.Warn(id, service, "osb: could not remove a pool member: %v", err)
		}
	}()
}

// fill keeps one pool at its size until ctx is done. Members are made concurrently, but no more
// than poolConcurrency at once across every pool: a refill is background work, and a hundred
// `docker run`s fired together would slow down the very creates it is refilling for.
func (s *Server) fill(ctx context.Context, p *pool) {
	for {
		p.mu.Lock()
		need := p.spec.Size - len(p.ready) - p.filling
		backoff := time.Duration(0)

		if p.failures > 0 {
			backoff = min(time.Duration(p.failures)*2*time.Second, time.Minute)
		}
		p.mu.Unlock()

		if backoff > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
		}

		for range max(need, 0) {
			select {
			case <-ctx.Done():
				return
			case s.poolSem <- struct{}{}:
			}

			p.mu.Lock()
			p.filling++
			p.mu.Unlock()

			s.wg.Add(1)

			go func() {
				defer s.wg.Done()
				defer func() { <-s.poolSem }()

				ok := s.addMember(ctx, p)

				p.mu.Lock()
				p.filling--

				if ok {
					p.failures = 0
				} else {
					p.failures++
				}
				p.mu.Unlock()

				p.poke()
			}()
		}

		select {
		case <-ctx.Done():
			return
		case <-p.wake:
		}
	}
}

// addMember makes one member: an ordinary create with the pool's template, then frozen. It
// reports whether the member joined the pool.
func (s *Server) addMember(ctx context.Context, p *pool) bool {
	pl, status, _, msg := s.validate(p.template)
	if status != 0 {
		logs.Default.Error("", "", "osb: pool %s: %s", p.spec.Image, msg)
		return false
	}

	pl.rec.Pool = p.key
	pl.rec.Reason, pl.rec.Message = "pool", "a warm-pool member; invisible until claimed"

	id := pl.rec.ID

	s.mu.Lock()
	s.recs[id] = &pl.rec

	if err := s.store.save(&pl.rec); err != nil {
		delete(s.recs, id)
		s.mu.Unlock()
		logs.Default.Error(id, service, "osb: pool %s: could not record a member under %s: %v",
			p.spec.Image, s.store.dir, err)

		return false
	}

	mctx, cancel := context.WithCancel(ctx)
	s.provisioning[id] = cancel
	s.mu.Unlock()

	defer cancel()

	s.provision(mctx, pl)

	rec, ok := s.snapshot(id)
	if !ok || ctx.Err() != nil {
		return false
	}

	if rec.State != stateRunning {
		logs.Default.Warn(id, service, "osb: pool %s: a member failed to start (%s: %s)",
			p.spec.Image, rec.Reason, rec.Message)
		s.discard(id)

		return false
	}

	units, err := s.p.List(ctx, id)
	if err != nil || len(units) == 0 || len(units[0].Client) == 0 {
		s.discard(id)
		return false
	}

	// Frozen and held: a member costs no CPU while it waits, and nothing that connects to its
	// port - nobody should, it has never been handed out - can thaw it.
	if err := s.rt.Freeze(ctx, id); err != nil {
		logs.Default.Warn(id, service, "osb: pool %s: could not freeze a member: %v", p.spec.Image, err)
		s.discard(id)

		return false
	}

	p.mu.Lock()
	p.ready = append(p.ready, poolMember{id: id, token: rec.Token, addr: units[0].Client[0].String()})
	p.mu.Unlock()

	return true
}

// drainPools removes every member that was never claimed. Called on the way out: a member is
// only useful to the process that holds its token, and left behind it is a frozen container
// nobody will ever claim.
func (s *Server) drainPools(ctx context.Context) {
	s.mu.Lock()

	var ids []string

	for id, r := range s.recs {
		if r.Pool != "" {
			ids = append(ids, id)
		}
	}
	s.mu.Unlock()

	if len(ids) == 0 {
		return
	}

	var wg sync.WaitGroup

	sem := make(chan struct{}, 8)

	for _, id := range ids {
		wg.Add(1)

		sem <- struct{}{}

		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			if err := s.remove(ctx, id, "pool"); err != nil {
				logs.Default.Warn(id, service, "osb: could not remove a pool member on the way out: %v - "+
					"`sbx rm %s` removes it", err, id)
			}
		}()
	}

	wg.Wait()
}

type poolStatus struct {
	Image   string `json:"image"`
	Size    int    `json:"size"`
	Ready   int    `json:"ready"`
	Filling int    `json:"filling"`
}

// poolStatus serves GET /sbx/v1/pool: how full each pool is. An sbx extension, outside /v1, for
// operators and for the benchmark, which waits for a full pool before it measures.
func (s *Server) poolStatus(w http.ResponseWriter, _ *http.Request) {
	out := []poolStatus{}

	for _, p := range s.pools {
		p.mu.Lock()
		out = append(out, poolStatus{Image: p.spec.Image, Size: p.spec.Size, Ready: len(p.ready), Filling: p.filling})
		p.mu.Unlock()
	}

	slices.SortFunc(out, func(a, b poolStatus) int { return strings.Compare(a.Image, b.Image) })

	writeJSON(w, http.StatusOK, out)
}
