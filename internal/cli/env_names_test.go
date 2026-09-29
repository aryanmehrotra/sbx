package cli

// Env-name collisions, said where they are made rather than only where they bite.
//
// `sbx env` warns when two services derive one name or an export already holds it. But the
// collision is made earlier: `sbx add s my-cache` beside an existing `my.cache` succeeded in
// silence, and MY_CACHE_PORT - which worked a moment before - vanished from the next `sbx env`.
// And a spec whose own services collide says nothing until somebody runs `sbx env` against a
// created sandbox. Both are warnings, not refusals: the sandbox works, one name is withheld.

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

const exportsYAtRedis = `{
  "version": 1,
  "services": {"redis": {"image": "redis:7-alpine", "ports": [6379], "health": "redis-cli ping"}},
  "exports": {"Y_PORT": "redis:6379"}
}`

func withMyDotCache() *raceStub {
	p := newRaceStub()
	p.units["s"] = []provider.Unit{
		{Sandbox: "s", Service: "redis", Ref: "sbx-s-redis", Instance: "a", Running: true,
			Client: []provider.Endpoint{{Host: "127.0.0.1", Port: 20000}}},
		{Sandbox: "s", Service: "my.cache", Ref: "sbx-s-my.cache", Instance: "b", Running: true, Index: 1,
			Client: []provider.Endpoint{{Host: "127.0.0.1", Port: 20001}}},
	}

	return p
}

func TestAddWarnsWhenItMakesACollision(t *testing.T) {
	cases := []struct {
		name, service string
		says          []string // empty: no warning at all
	}{
		{"beside my.cache", "my-cache", []string{`"my-cache"`, `"my.cache"`, "MY_CACHE_PORT", "another name"}},
		{"under an export's name", "y", []string{`"y"`, "Y_PORT", "an export already has that name", "another name"}},
		{"nothing collides", "other", nil},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			warned := captureStderr(t)

			p := withMyDotCache()

			err := Add(context.Background(), p, writeSpec(t, exportsYAtRedis), "s", c.service, "redis:7-alpine",
				[]int{6379}, "true", nil, "", nil, provider.IsolationContainer)
			if err != nil {
				t.Fatalf("a collision is a warning, not a refusal: %v", err)
			}

			if !p.has("s") || len(p.units["s"]) != 3 {
				t.Fatalf("the service was not added: %v", p.units["s"])
			}

			if len(c.says) == 0 {
				if warned.Len() != 0 {
					t.Errorf("warned with nothing colliding:\n%s", warned.String())
				}

				return
			}

			for _, want := range c.says {
				if !strings.Contains(warned.String(), want) {
					t.Errorf("the warning does not say %q:\n%s", want, warned.String())
				}
			}
		})
	}
}

// Only what this add caused: a collision that was already there is `sbx env`'s to report, and
// repeating it on every add teaches people to skip the line.
func TestAddDoesNotRepeatACollisionItDidNotMake(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	warned := captureStderr(t)

	p := withMyDotCache()
	p.units["s"] = append(p.units["s"], provider.Unit{Sandbox: "s", Service: "my_cache", Ref: "sbx-s-my_cache",
		Instance: "c", Running: true, Index: 2, Client: []provider.Endpoint{{Host: "127.0.0.1", Port: 20002}}})

	if err := Add(context.Background(), p, writeSpec(t, exportsYAtRedis), "s", "other", "redis:7-alpine",
		[]int{6379}, "true", nil, "", nil, provider.IsolationContainer); err != nil {
		t.Fatal(err)
	}

	if warned.Len() != 0 {
		t.Errorf("an unrelated add repeated an old collision:\n%s", warned.String())
	}
}

// Two services in the spec that derive one name, and an export named like a service's derived
// name, are visible from the file alone - so validate says so, and still passes.
const collidingSpec = `{
  "version": 1,
  "services": {
    "x":        {"image": "redis:7-alpine", "ports": [6379], "health": "true"},
    "y":        {"image": "redis:7-alpine", "ports": [6380], "health": "true"},
    "my.cache": {"image": "redis:7-alpine", "ports": [6381], "health": "true"},
    "my-cache": {"image": "redis:7-alpine", "ports": [6382], "health": "true"}
  },
  "exports": {"Y_PORT": "x:6379"}
}`

func TestValidateWarnsOnCollisionsInTheSpec(t *testing.T) {
	warned := captureStderr(t)

	var out bytes.Buffer
	if err := Validate(&out, writeSpec(t, collidingSpec)); err != nil {
		t.Fatalf("a collision is a warning, not a refusal: %v", err)
	}

	if !strings.Contains(out.String(), "valid.") {
		t.Errorf("validate did not pass the spec:\n%s", out.String())
	}

	for _, want := range []string{`"my-cache"`, `"my.cache"`, "MY_CACHE_PORT", `"y"`, "Y_PORT", "`exports` entry"} {
		if !strings.Contains(warned.String(), want) {
			t.Errorf("the warning does not say %q:\n%s", want, warned.String())
		}
	}

	// Warnings go to stderr: validate's stdout is read by linters.
	if strings.Contains(out.String(), "warning") {
		t.Errorf("a warning reached stdout:\n%s", out.String())
	}
}

func TestValidateIsQuietOnAClashFreeSpec(t *testing.T) {
	warned := captureStderr(t)

	if err := Validate(&bytes.Buffer{}, writeSpec(t, exportsYAtRedis)); err != nil {
		t.Fatal(err)
	}

	if warned.Len() != 0 {
		t.Errorf("warned about a spec with no collision:\n%s", warned.String())
	}
}

func TestCreateWarnsOnCollisionsInTheSpec(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	warned := captureStderr(t)

	var err error

	_ = captureOutput(t, func() {
		err = Create(context.Background(), newRaceStub(), writeSpec(t, collidingSpec), "c", false, provider.IsolationContainer)
	})
	if err != nil {
		t.Fatalf("a collision is a warning, not a refusal: %v", err)
	}

	if !strings.Contains(warned.String(), "MY_CACHE_PORT") || !strings.Contains(warned.String(), "Y_PORT") {
		t.Errorf("create did not warn about the spec's collisions:\n%s", warned.String())
	}
}
