package spec

// The sandbox spec: what a repo declares so that anyone — a person, an agent, CI — can
// have their own copy of its backing services.
//
// A spec never says when to start or stop anything. It says what exists, how to tell when
// it is serving, and how to reach it. Lifecycle belongs to `sbx serve`, which watches the
// ports; if a spec could start something, the spec would eventually be what left it running.

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Spec is the on-disk sandbox.json.
type Spec struct {
	// Version lets a future format change be detected rather than silently misread.
	Version int `json:"version"`

	// Services is the declared set. Map rather than list so a service has one obvious
	// name and merging an ad-hoc addition is unambiguous.
	Services map[string]Service `json:"services"`

	// Exports turns port assignments into the environment variables a repo's own tooling
	// already reads: {"DB_PORT": "mysql:3306"} becomes DB_PORT=<public port of mysql 3306>.
	// Without this, adopting sbx would mean changing every script that knows a port.
	Exports map[string]string `json:"exports,omitempty"`
}

// Service is one wakeable container.
type Service struct {
	Image string `json:"image"`

	// Ports are container-side ports to expose. Public and backing ports are assigned
	// from the sandbox's slot, never chosen here: two repos that both picked 5432 would
	// collide the moment someone opened both.
	Ports []int `json:"ports"`

	// Health is a command run inside the container. Strongly recommended: without it the
	// daemon can only dial the published port, which docker answers before the server
	// does — so the first query after a wake lands on a socket that is about to close.
	Health string `json:"health,omitempty"`

	Env  map[string]string `json:"env,omitempty"`
	Args []string          `json:"args,omitempty"`

	// Volume is a container path to persist. One per service, named after the sandbox, so
	// that sleeping is safe and destroying is deliberate.
	Volume string `json:"volume,omitempty"`

	// Files mounts read-only host files, relative to the spec: tuned configs live in the
	// repo next to the spec that references them.
	Files map[string]string `json:"files,omitempty"`

	// Init runs once, after the service first reports healthy — schemas, users, seed data.
	// Not on every start: a woken container already has whatever this created.
	Init []string `json:"init,omitempty"`

	// Optional keeps a heavy service out of the default sandbox. A branch that never
	// queries the analytics store should not pay for one.
	Optional bool `json:"optional,omitempty"`
}

func (s Service) validate(name string) error {
	if strings.TrimSpace(s.Image) == "" {
		return fmt.Errorf("service %q: image is required", name)
	}

	if len(s.Ports) == 0 {
		return fmt.Errorf("service %q: at least one port is required", name)
	}

	for _, p := range s.Ports {
		if p < 1 || p > 65535 {
			return fmt.Errorf("service %q: port %d out of range", name, p)
		}
	}

	return nil
}

// LoadSpec reads and validates a sandbox.json.
func LoadSpec(path string) (*Spec, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	return ParseSpec(raw, path)
}

func ParseSpec(raw []byte, path string) (*Spec, error) {
	var s Spec
	// DisallowUnknownFields: a typo in a spec should be a startup error, not a setting
	// that silently did nothing for a week.
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()

	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	if s.Version != 1 {
		return nil, fmt.Errorf("%s: unsupported version %d (this build understands 1)", path, s.Version)
	}

	if len(s.Services) == 0 {
		return nil, fmt.Errorf("%s: no services declared", path)
	}

	for name, svc := range s.Services {
		if err := svc.validate(name); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}

	for env, ref := range s.Exports {
		svc, port, ok := strings.Cut(ref, ":")
		if !ok {
			return nil, fmt.Errorf("%s: export %s: want \"service:port\", got %q", path, env, ref)
		}

		if _, exists := s.Services[svc]; !exists {
			return nil, fmt.Errorf("%s: export %s refers to unknown service %q", path, env, svc)
		}

		if !HasPort(s.Services[svc], port) {
			return nil, fmt.Errorf("%s: export %s refers to port %s, which %s does not expose", path, env, port, svc)
		}
	}

	return &s, nil
}

func HasPort(s Service, want string) bool {
	for _, p := range s.Ports {
		if fmt.Sprint(p) == want {
			return true
		}
	}

	return false
}

// names returns service names in a stable order, so output and slot assignment do not
// shuffle between runs for no reason.
func (s *Spec) Names() []string {
	out := make([]string, 0, len(s.Services))
	for n := range s.Services {
		out = append(out, n)
	}

	sort.Strings(out)

	return out
}

// ── layout ───────────────────────────────────────────────────────────────────
//
// Services get an ordinal, not a port. Which address an ordinal becomes is the provider's
// decision: on one machine it indexes into a per-slot port block, and in a cluster it is
// ignored entirely because a pod has its own address.

// MaxOrdinals bounds how many ports one sandbox may declare. It exists here because the
// spec is where the limit is enforced; what an ordinal becomes is decided elsewhere.
const MaxOrdinals = 20

type SlotIndex struct {
	Service   string
	Container int // port inside the container
	Index     int // ordinal within the sandbox
}

// assign lays out every service deterministically.
//
// Every declared service gets an ordinal, including optional ones that were not created.
// Reserving them costs nothing and keeps the layout stable: if skipping ClickHouse shifted
// MySQL, then adding ClickHouse later would silently move the database out from under every
// config that had already recorded where it was.
func (s *Spec) Assign() ([]SlotIndex, error) {
	var (
		out  []SlotIndex
		next = 0
	)

	for _, name := range s.Names() {
		for _, cp := range s.Services[name].Ports {
			if next >= MaxOrdinals {
				return nil, fmt.Errorf("a sandbox declares more than %d ports", MaxOrdinals)
			}

			out = append(out, SlotIndex{Service: name, Container: cp, Index: next})
			next++
		}
	}

	return out, nil
}

// startIndex is the ordinal a service's first port takes.
func (s *Spec) StartIndex(layout []SlotIndex, service string) (int, bool) {
	for _, a := range layout {
		if a.Service == service {
			return a.Index, true
		}
	}

	return 0, false
}
