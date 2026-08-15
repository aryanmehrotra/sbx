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
// Build describes an image to build instead of pull.
//
// The alternative today is that everyone writes their own Dockerfile, builds it by hand,
// tags it themselves and remembers to rebuild it — which is a build system every user
// reimplements badly. Daytona ships one and caches it; this is the same idea with the
// caching done by content rather than by a clock.
type Build struct {
	// Context is the directory to build, relative to the spec.
	Context string `json:"context"`

	// Dockerfile is relative to Context. Empty means "Dockerfile".
	Dockerfile string `json:"dockerfile,omitempty"`
}

type Service struct {
	// Image is what to run. Exactly one of Image or Build is required — an image with no
	// build is a pull, a build with no image is built and tagged by its own content.
	Image string `json:"image"`

	// Build makes the image instead of pulling it.
	Build *Build `json:"build,omitempty"`

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

	// Egress says whether this service may reach the network beyond the host.
	//
	//	""      unset — whatever the backend does by default, which is what every
	//	        existing spec already gets
	//	"deny"  no routed egress. It can still be reached, and can still talk to the
	//	        rest of its own sandbox
	//
	// Named for the intent, not the mechanism: docker does it with a bridge that has IP
	// masquerade disabled, a cluster does it with a NetworkPolicy, and a backend that
	// cannot do it at all must refuse rather than quietly leave the service open.
	//
	// It is not a domain allow-list. Docker has no primitive for that and doing it
	// properly needs a filtering proxy in the data path — a component with a lifecycle,
	// not a flag. Claiming it with anything less would be a control that does not control.
	Egress string `json:"egress,omitempty"`

	// CPU and Memory cap what one service may take, passed to the runtime verbatim:
	// CPU is cores ("0.5", "2"), Memory is a size ("512m", "2g").
	//
	// Unset means unlimited, which is what every sandbox had before this existed and is
	// fine for one. It stops being fine at twenty: a laptop running a sandbox per branch
	// has no ceiling at all, and the failure is the machine rather than the sandbox — the
	// limit that binds first, long before any wake latency does.
	//
	// Not validated here. Docker and Kubernetes each reject their own malformed values
	// with a better message than this could paraphrase, and unlike `egress` a typo here
	// fails loudly at create rather than silently leaving something open.
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`

	// GPUs is passed to the runtime verbatim: "all", "1", "device=0". Empty means none.
	// Declared here rather than inferred, because a sandbox that quietly grabs every GPU
	// on a shared machine is a bad neighbour.
	GPUs string `json:"gpus,omitempty"`
}

func (s Service) validate(name string) error {
	hasImage, hasBuild := strings.TrimSpace(s.Image) != "", s.Build != nil

	switch {
	case !hasImage && !hasBuild:
		return fmt.Errorf("service %q: needs an image or a build", name)
	case hasImage && hasBuild:
		// Refused rather than picking one. Which of the two wins is exactly the kind of
		// thing a reader would guess wrong, and guessing here means running a different
		// image than the file appears to describe.
		return fmt.Errorf("service %q: has both image and build — one or the other, "+
			"since a built image is tagged from its own content", name)
	case hasBuild && strings.TrimSpace(s.Build.Context) == "":
		return fmt.Errorf("service %q: build needs a context directory", name)
	}

	if len(s.Ports) == 0 {
		return fmt.Errorf("service %q: at least one port is required", name)
	}

	for _, p := range s.Ports {
		if p < 1 || p > 65535 {
			return fmt.Errorf("service %q: port %d out of range", name, p)
		}
	}

	// A typo in a security control must fail rather than silently leave egress open.
	// "den" is not "deny", and the difference is a sandbox that can reach the internet.
	switch s.Egress {
	case "", EgressDeny:
	default:
		return fmt.Errorf("service %q: egress %q is not valid — the only value is %q",
			name, s.Egress, EgressDeny)
	}

	return nil
}

// EgressDeny is the only egress value, because "allow" is the absence of the field.
const EgressDeny = "deny"

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
