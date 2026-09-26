package provider

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/egress"
	"github.com/aryanmehrotra/sbx/internal/fc"
	"github.com/aryanmehrotra/sbx/internal/spec"
)

// initExt4 is touchExt4 that keeps the /init.json each agent drive was built with, so a test
// can see the environment a guest boots into.
type initExt4 struct {
	touchExt4

	mu    sync.Mutex
	inits []fc.InitConfig
}

func (e *initExt4) Build(ctx context.Context, src, img, label string) error {
	if label == "sbxagent" {
		b, err := os.ReadFile(filepath.Join(src, "init.json"))
		if err != nil {
			return err
		}

		var cfg fc.InitConfig
		if err := json.Unmarshal(b, &cfg); err != nil {
			return err
		}

		e.mu.Lock()
		e.inits = append(e.inits, cfg)
		e.mu.Unlock()
	}

	return e.touchExt4.Build(ctx, src, img, label)
}

func envValues(env []string, key string) []string {
	var out []string

	for _, kv := range env {
		if k, v, _ := strings.Cut(kv, "="); k == key {
			out = append(out, v)
		}
	}

	return out
}

func TestAFilteredMicroVMIsPointedAtItsBridgeGatewayFilter(t *testing.T) {
	r := newRig(t)
	ext := &initExt4{}
	r.p.ext4 = ext
	r.g.available = false

	svc := redis
	svc.EgressAllow = []string{"example.com"}
	svc.Env = map[string]string{"A": "1", "HTTPS_PROXY": "http://elsewhere:3128"}

	ref := r.create(t, "eg1", svc)

	if len(ext.inits) != 1 {
		t.Fatalf("agent drives built: %d", len(ext.inits))
	}

	env := ext.inits[0].Env
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		// Exactly once, and the filter: a proxy the spec named is a route this bridge lacks.
		if got := envValues(env, k); !slices.Equal(got, []string{"http://10.231.0.1:20999"}) {
			t.Errorf("%s = %v, want the filter on the bridge gateway once", k, got)
		}
	}

	if got := envValues(env, "A"); !slices.Equal(got, []string{"1"}) {
		t.Errorf("the spec's own env was lost: A = %v", got)
	}

	vm := r.vm(t, ref)
	if vm.EgressPolicy == "" {
		t.Fatal("the record does not say the VM is filtered")
	}

	units, err := r.p.List(r.ctx, "eg1")
	if err != nil || len(units) != 1 {
		t.Fatalf("List = %v, %v", units, err)
	}

	u := units[0]
	if u.EgressGateway != "10.231.0.1" || u.EgressBridge != "sbxfc0" || u.EgressStat != "" {
		t.Fatalf("unit egress = gateway %q bridge %q stat %q", u.EgressGateway, u.EgressBridge, u.EgressStat)
	}

	want := egress.FromAllowList([]string{"example.com"})
	if got := DeclaredPolicy(units); got.Hash() != want.Hash() {
		t.Fatalf("declared = %+v, want %+v", got, want)
	}

	var _ EgressFilters = r.p // the daemon's EgressControl finds it through this

	f, err := r.p.EgressFilter(r.ctx, "eg1")
	if err != nil {
		t.Fatal(err)
	}

	if f.Gateway != "10.231.0.1" || !slices.Equal(f.Services, []string{"cache"}) ||
		f.Control != "" || f.Declared.Hash() != want.Hash() {
		t.Fatalf("EgressFilter = %+v", f)
	}
}

func TestEgressAllowOnAMicroVMIsAnOpenDefaultThroughTheFilter(t *testing.T) {
	r := newRig(t)
	r.g.available = false

	svc := redis
	svc.Egress = spec.EgressAllow

	r.create(t, "eg2", svc)

	units, err := r.p.List(r.ctx, "eg2")
	if err != nil || len(units) != 1 || units[0].EgressGateway == "" {
		t.Fatalf("List = %+v, %v", units, err)
	}

	if p := DeclaredPolicy(units); p.DefaultAction != egress.ActionAllow || len(p.Egress) != 0 {
		t.Fatalf("declared = %+v, want allow with no rules", p)
	}
}

func TestAnUnfilteredMicroVMHasNoProxyAndNoFilter(t *testing.T) {
	for _, e := range []string{"", spec.EgressDeny} {
		r := newRig(t)
		ext := &initExt4{}
		r.p.ext4 = ext
		r.g.available = false

		svc := redis
		svc.Egress = e
		r.create(t, "eg3", svc)

		if got := envValues(ext.inits[0].Env, "HTTP_PROXY"); len(got) != 0 {
			t.Errorf("egress %q: HTTP_PROXY = %v", e, got)
		}

		units, _ := r.p.List(r.ctx, "eg3")
		if units[0].EgressGateway != "" || units[0].EgressBridge != "" {
			t.Errorf("egress %q: unit reports a filter: %+v", e, units[0])
		}

		if _, err := r.p.EgressFilter(r.ctx, "eg3"); !errors.Is(err, ErrNotFiltered) {
			t.Errorf("egress %q: EgressFilter = %v, want ErrNotFiltered", e, err)
		}
	}

	r := newRig(t)
	if _, err := r.p.EgressFilter(r.ctx, "nope"); !errors.Is(err, ErrNoSandbox) {
		t.Fatalf("EgressFilter(absent) = %v", err)
	}
}

func TestEveryFilteredEgressFormIsAcceptedOnAMicroVM(t *testing.T) {
	pol := egress.Policy{DefaultAction: egress.ActionDeny, Egress: []egress.Rule{{Action: egress.ActionAllow, Target: "*.example.com"}}}

	for name, svc := range map[string]spec.Service{
		"egress_allow":  {Image: "x", EgressAllow: []string{"example.com"}},
		"egress_policy": {Image: "x", EgressPolicy: &pol},
		"egress allow":  {Image: "x", Egress: spec.EgressAllow},
		"egress deny":   {Image: "x", Egress: spec.EgressDeny},
	} {
		if err := unsupported(svc); err != nil {
			t.Errorf("%s refused: %v", name, err)
		}
	}

	// A value this provider has no meaning for is still refused, never ignored.
	if err := unsupported(spec.Service{Image: "x", Egress: "open"}); err == nil || !strings.Contains(err.Error(), "egress") {
		t.Fatalf("egress: open = %v", err)
	}
}

// A snapshot holds the proxy setting in the memory of every process in it, so it comes back
// only with the same filtered-or-not it was taken with. The policy itself may change.
func TestASnapshotRestoresOnlyWithTheEgressItWasTakenWith(t *testing.T) {
	r := newRig(t)

	svc := redis
	svc.EgressAllow = []string{"example.com"}
	ref := r.create(t, "eg4", svc)

	if err := r.p.Start(r.ctx, ref); err != nil {
		t.Fatal(err)
	}

	const img = "sbx-snap-eg4-cache:latest"
	if err := r.p.Commit(r.ctx, ref, img); err != nil {
		t.Fatal(err)
	}

	slot := r.vm(t, ref).Slot
	if err := r.p.Remove(r.ctx, "eg4"); err != nil {
		t.Fatal(err)
	}

	eps := r.p.Endpoints("eg4", "cache", slot, 0, redis.Ports)

	plain := redis
	plain.Image = img

	if err := r.p.Create(r.ctx, "eg4", slot, 0, "cache", plain, eps, "", IsolationContainer); err == nil ||
		!strings.Contains(err.Error(), "egress filter") {
		t.Fatalf("filtered snapshot restored unfiltered: %v", err)
	}

	changed := plain
	changed.EgressAllow = []string{"other.example"}

	if err := r.p.Create(r.ctx, "eg4", slot, 0, "cache", changed, eps, "", IsolationContainer); err != nil {
		t.Fatal(err)
	}

	units, _ := r.p.List(r.ctx, "eg4")
	if want := egress.FromAllowList([]string{"other.example"}); DeclaredPolicy(units).Hash() != want.Hash() {
		t.Fatalf("restored with the snapshot's policy, not the spec's: %+v", DeclaredPolicy(units))
	}
}
