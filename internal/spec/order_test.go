package spec

import (
	"strings"
	"testing"
)

func specOf(deps map[string][]string) *Spec {
	s := &Spec{Version: 1, Services: map[string]Service{}}
	for name, d := range deps {
		s.Services[name] = Service{Image: "x", Ports: []int{1}, DependsOn: d}
	}

	return s
}

func indexIn(xs []string, want string) int {
	for i, x := range xs {
		if x == want {
			return i
		}
	}

	return -1
}

// The case this exists for: an app that dials its database at boot, created before the
// database because "api" sorts before "postgres".
func TestADependencyIsCreatedFirst(t *testing.T) {
	s := specOf(map[string][]string{
		"api":      {"postgres"},
		"postgres": nil,
	})

	order, err := s.CreationOrder()
	if err != nil {
		t.Fatalf("CreationOrder: %v", err)
	}

	if indexIn(order, "postgres") > indexIn(order, "api") {
		t.Errorf("api is created before the postgres it depends on: %v", order)
	}
}

// Transitively, and deterministically. A spec that creates in a different order on different
// runs makes a failure impossible to reproduce.
func TestOrderIsTransitiveAndStable(t *testing.T) {
	s := specOf(map[string][]string{
		"web":      {"api"},
		"api":      {"postgres", "redis"},
		"postgres": nil,
		"redis":    nil,
	})

	first, err := s.CreationOrder()
	if err != nil {
		t.Fatalf("CreationOrder: %v", err)
	}

	for _, pair := range [][2]string{{"postgres", "api"}, {"redis", "api"}, {"api", "web"}} {
		if indexIn(first, pair[0]) > indexIn(first, pair[1]) {
			t.Errorf("%s must come before %s: %v", pair[0], pair[1], first)
		}
	}

	for range 20 {
		again, err := s.CreationOrder()
		if err != nil {
			t.Fatal(err)
		}

		if strings.Join(again, ",") != strings.Join(first, ",") {
			t.Fatalf("order is not deterministic:\n  %v\n  %v", first, again)
		}
	}
}

// No dependencies at all must behave exactly as before this feature existed, or adding it
// changed the behaviour of every spec that does not use it.
func TestNoDependenciesIsStillAlphabetical(t *testing.T) {
	s := specOf(map[string][]string{"redis": nil, "postgres": nil, "chrome": nil})

	order, err := s.CreationOrder()
	if err != nil {
		t.Fatalf("CreationOrder: %v", err)
	}

	want := "chrome,postgres,redis"
	if got := strings.Join(order, ","); got != want {
		t.Errorf("order = %q, want %q", got, want)
	}
}

// A cycle has no valid answer, so it must be refused rather than silently resolved into
// whichever order the traversal happened to produce.
func TestACycleIsRefused(t *testing.T) {
	s := specOf(map[string][]string{"a": {"b"}, "b": {"c"}, "c": {"a"}})

	if _, err := s.CreationOrder(); err == nil {
		t.Fatal("a dependency cycle was accepted")
	}

	if _, err := specOf(map[string][]string{"a": {"a"}}).CreationOrder(); err == nil {
		t.Fatal("a service depending on itself was accepted")
	}
}

// A typo in a dependency name would otherwise be a rule that silently never applied —
// indistinguishable from the race it was added to prevent, and much harder to find.
func TestADependencyOnSomethingUndeclaredIsRefused(t *testing.T) {
	s := specOf(map[string][]string{"api": {"postgress"}, "postgres": nil})

	_, err := s.CreationOrder()
	if err == nil {
		t.Fatal("a dependency on an undeclared service was accepted")
	}

	for _, want := range []string{"postgress", "postgres"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
}

// Dependencies must not move ports. Somebody's DATABASE_PORT changing because a colleague
// added a depends_on line would be a far worse bug than the race it fixes.
func TestDependenciesDoNotChangeOrdinals(t *testing.T) {
	plain := specOf(map[string][]string{"api": nil, "postgres": nil, "redis": nil})
	withDeps := specOf(map[string][]string{"api": {"postgres", "redis"}, "postgres": nil, "redis": nil})

	a, err := plain.Assign()
	if err != nil {
		t.Fatal(err)
	}

	b, err := withDeps.Assign()
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range plain.Names() {
		x, _ := plain.StartIndex(a, name)
		y, _ := withDeps.StartIndex(b, name)

		if x != y {
			t.Errorf("%s moved from ordinal %d to %d because a dependency was declared — "+
				"that changes the port an existing sandbox exports", name, x, y)
		}
	}
}

// A required service depending on an optional one is only a problem when the optional one is
// skipped — which is the default. Create refuses it there rather than bringing up a service
// without the dependency it declared, because that would be the exact failure depends_on
// exists to prevent with the cause moved from "alphabetical accident" to "optional accident".
//
// Ordering itself stays neutral: it returns every declared service, and whether one is
// actually created is not something a topological sort should be deciding.
func TestOrderingIncludesOptionalServices(t *testing.T) {
	s := &Spec{Version: 1, Services: map[string]Service{
		"api":       {Image: "x", Ports: []int{1}, DependsOn: []string{"analytics"}},
		"analytics": {Image: "y", Ports: []int{2}, Optional: true},
	}}

	order, err := s.CreationOrder()
	if err != nil {
		t.Fatalf("CreationOrder: %v", err)
	}

	if len(order) != 2 {
		t.Fatalf("order dropped a service: %v", order)
	}

	if indexIn(order, "analytics") > indexIn(order, "api") {
		t.Errorf("an optional dependency was not ordered first: %v", order)
	}
}
