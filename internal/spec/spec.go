package spec

// The sandbox spec: what a repo declares so that anyone - a person, an agent, CI - can
// have their own copy of its backing services.
//
// A spec never says when to start or stop anything. It says what exists, how to tell when
// it is serving, and how to reach it. Lifecycle belongs to `sbx serve`, which watches the
// ports; if a spec could start something, the spec would eventually be what left it running.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
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

	// HealthInterval is how often every service in this sandbox is asked whether it is
	// serving, unless it says otherwise. Empty means the default - see DefaultHealthInterval.
	//
	// Here as well as per service because the cost is a property of the sandbox rather than of
	// any one thing in it: the probes run whether or not anybody is waiting, so a sandbox of
	// fourteen services at the default interval is running about forty-seven commands a second
	// inside containers, for ever. On a laptop whose runtime is already busy that is worth
	// turning down once for the whole file rather than fourteen times.
	HealthInterval string `json:"health_interval,omitempty"`
}

// checkInterval refuses a probe interval that would not do what the writer meant.
//
// Both ends matter. Below about fifty milliseconds the probe is a command started inside a
// container more often than most containers can answer one, which spends cpu to learn nothing;
// above a few minutes the daemon would report a service as still waking long after it was
// serving, and a wake that appears to take four minutes reads as a hang.
func checkInterval(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}

	d, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("%q is not a duration - try \"1s\" or \"500ms\"", raw)
	}

	switch {
	case d < 50*time.Millisecond:
		return fmt.Errorf("%s is faster than a container can usefully answer; 50ms is the floor", d)
	case d > 5*time.Minute:
		return fmt.Errorf("%s would report a service as still waking long after it was serving; "+
			"5m is the ceiling", d)
	}

	return nil
}

// ProbeInterval is how often this service should be asked whether it is serving: its own
// setting, else the sandbox's, else the default.
//
// Resolved in one place because three defaults resolved at three call sites is how docker and
// kubernetes come to disagree about what the file said.
func (s Spec) ProbeInterval(svc Service) time.Duration {
	for _, raw := range []string{svc.HealthInterval, s.HealthInterval} {
		if strings.TrimSpace(raw) == "" {
			continue
		}

		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
	}

	return DefaultHealthInterval
}

// DefaultHealthInterval is how often a service is asked whether it is serving.
//
// It is the floor on how long a wake appears to take, because a wake is not over until the
// answer changes and the answer is only re-evaluated on this interval. Three hundred
// milliseconds is chosen against that: a wake that is genuinely ready in 40 ms should not be
// reported as taking a second, and the probe is a command run inside a container, which is not
// free. Raise it on a machine with many services or a slow runtime; lower it if you are
// measuring wakes and want the resolution.
const DefaultHealthInterval = 300 * time.Millisecond

// Service is one wakeable container.
// Build describes an image to build instead of pull.
//
// The alternative today is that everyone writes their own Dockerfile, builds it by hand,
// tags it themselves and remembers to rebuild it - which is a build system every user
// reimplements badly. Daytona ships one and caches it; this is the same idea with the
// caching done by content rather than by a clock.
type Build struct {
	// Context is the directory to build, relative to the spec.
	Context string `json:"context"`

	// Dockerfile is relative to Context. Empty means "Dockerfile".
	Dockerfile string `json:"dockerfile,omitempty"`
}

type Service struct {
	// Image is what to run. Exactly one of Image or Build is required - an image with no
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
	// does - so the first query after a wake lands on a socket that is about to close.
	Health string `json:"health,omitempty"`

	// HealthInterval overrides the sandbox's own, for a service whose probe is expensive or
	// whose readiness is worth catching quickly.
	HealthInterval string `json:"health_interval,omitempty"`

	Env  map[string]string `json:"env,omitempty"`
	Args []string          `json:"args,omitempty"`

	// Volume is a container path to persist. One per service, named after the sandbox, so
	// that sleeping is safe and destroying is deliberate.
	Volume string `json:"volume,omitempty"`

	// Files mounts read-only host files, relative to the spec: tuned configs live in the
	// repo next to the spec that references them.
	Files map[string]string `json:"files,omitempty"`

	// Mounts binds host directories read-write, host path to container path, relative to the
	// spec like Files.
	//
	// Three things already persist state and none of them is this. `volume` is a named volume
	// the runtime owns, which survives sleeping and is destroyed with the sandbox - right for a
	// database's data directory, and deliberately not something you can open in an editor.
	// `files` is read-only, for configs. This is the third case: a directory on YOUR disk that
	// the service writes to and you can see - a source tree it rebuilds from, the dump it
	// produces, the fixtures a test run leaves behind.
	//
	// It is a laptop feature and says so where a backend cannot do it. A cluster's hostPath is
	// a node's disk rather than yours, which is a different thing wearing the same word, and
	// the kubernetes provider refuses rather than mounting somebody else's filesystem and
	// calling it success.
	Mounts map[string]string `json:"mounts,omitempty"`

	// Init runs once, after the service first reports healthy - schemas, users, seed data.
	// Not on every start: a woken container already has whatever this created.
	Init []string `json:"init,omitempty"`

	// DependsOn names services that must be serving before this one starts - at creation,
	// and again on every wake.
	//
	// It deliberately does not affect which port a service gets: ordinals stay alphabetical,
	// so adding a dependency never moves an existing sandbox's addresses.
	//
	// Wake order used to be excluded from this, on the reasoning that a service needing
	// another at runtime should retry. That reasoning does not survive contact with a
	// sleeping peer: a stopped container is not slow to answer, it is absent from the
	// network's DNS, so the dial fails with `no such host` and there is nothing to retry
	// towards. Measured on a fourteen-service sandbox, six services died that way within a
	// minute of their datastores being slept.
	//
	// So a wake now walks this first, in parallel across independent siblings, and a cycle
	// is broken rather than followed. Declaring nothing costs nothing: a service with no
	// dependencies takes exactly the path it always took, which is what keeps the published
	// wake numbers true for a single-service sandbox.
	DependsOn []string `json:"depends_on,omitempty"`

	// Optional keeps a heavy service out of the default sandbox. A branch that never
	// queries the analytics store should not pay for one.
	Optional bool `json:"optional,omitempty"`

	// Egress says whether this service may reach the network beyond the host.
	//
	//	""      unset - whatever the backend does by default, which is what every
	//	        existing spec already gets
	//	"deny"  no routed egress. It can still be reached, and can still talk to the
	//	        rest of its own sandbox
	//
	// Named for the intent, not the mechanism: docker does it with a bridge that has IP
	// masquerade disabled, a cluster does it with a NetworkPolicy, and a backend that
	// cannot do it at all must refuse rather than quietly leave the service open.
	//
	// For a domain allow-list rather than all-or-nothing, use EgressAllow.
	Egress string `json:"egress,omitempty"`

	// EgressAllow turns egress into an allow-list: the service reaches only these hosts and
	// nothing else. Each entry is a host or host:port and matches the host and its subdomains,
	// so "openai.com" permits api.openai.com. It is enforced by a filtering proxy sbx runs in
	// the data path - the direct route off the host is denied exactly as `egress: "deny"`
	// denies it, and the proxy is the one way out, so the list is enforced, not advisory.
	//
	// This is the component-with-a-lifecycle the Egress note calls for. Setting it implies deny
	// for everything unlisted, so it is not combined with egress: "deny" (which would deny the
	// allowed hosts too). An agent box that may reach an LLM API and its package registry, and
	// nothing else, is the case this exists for.
	EgressAllow []string `json:"egress_allow,omitempty"`

	// CPU and Memory cap what one service may take, passed to the runtime verbatim:
	// CPU is cores ("0.5", "2"), Memory is a size ("512m", "2g").
	//
	// Unset means unlimited, which is what every sandbox had before this existed and is
	// fine for one. It stops being fine at twenty: a laptop running a sandbox per branch
	// has no ceiling at all, and the failure is the machine rather than the sandbox - the
	// limit that binds first, long before any wake latency does.
	//
	// Not validated here. Docker and Kubernetes each reject their own malformed values
	// with a better message than this could paraphrase, and unlike `egress` a typo here
	// fails loudly at create rather than silently leaving something open.
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`

	// Idle overrides how long this service may go without traffic before the daemon sleeps it,
	// for the one case the byte-through-the-proxy signal misses: an agent doing work INSIDE the
	// box - a long exec, a compute loop, waiting on an API - sends nothing through sbx, so the
	// default idle timer would sleep the container and kill the work. "never" (or "0") keeps it
	// awake until it is explicitly slept or removed; a duration ("30m") sets a longer window.
	// Empty uses the daemon's global --idle.
	Idle string `json:"idle,omitempty"`

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
		return fmt.Errorf("service %q: has both image and build - one or the other, "+
			"since a built image is tagged from its own content", name)
	case hasBuild && strings.TrimSpace(s.Build.Context) == "":
		return fmt.Errorf("service %q: build needs a context directory", name)
	}

	if len(s.Ports) == 0 {
		return fmt.Errorf("service %q: at least one port is required", name)
	}

	if err := checkInterval(s.HealthInterval); err != nil {
		return fmt.Errorf("service %q: health_interval %w", name, err)
	}

	for _, p := range s.Ports {
		if p < 1 || p > 65535 {
			return fmt.Errorf("service %q: port %d out of range", name, p)
		}
	}

	// A mount that does not name a place is a mount that lands somewhere else. Docker reads a
	// relative container path as a *name* it invents rather than a directory, so the service
	// starts, the mount appears to have worked, and the files are nowhere the author meant.
	for host, container := range s.Mounts {
		if strings.TrimSpace(host) == "" {
			return fmt.Errorf("service %q: a mount has no host path", name)
		}

		if !strings.HasPrefix(container, "/") {
			return fmt.Errorf("service %q: mount %q -> %q needs an absolute container path, "+
				"because a relative one is a name the runtime invents rather than a place",
				name, host, container)
		}
	}

	// A typo in a security control must fail rather than silently leave egress open.
	// "den" is not "deny", and the difference is a sandbox that can reach the internet.
	switch s.Egress {
	case "", EgressDeny:
	default:
		return fmt.Errorf("service %q: egress %q is not valid - the only value is %q",
			name, s.Egress, EgressDeny)
	}

	if len(s.EgressAllow) > 0 {
		if s.Egress == EgressDeny {
			return fmt.Errorf("service %q: egress_allow and egress %q together deny even the "+
				"allowed hosts - use one, not both", name, EgressDeny)
		}

		for _, h := range s.EgressAllow {
			if strings.TrimSpace(h) == "" {
				return fmt.Errorf("service %q: egress_allow has a blank host", name)
			}
		}
	}

	if s.Idle != "" && !s.IdleNever() {
		if _, err := time.ParseDuration(s.Idle); err != nil {
			return fmt.Errorf("service %q: idle %q is not \"never\", \"0\", or a duration "+
				"like \"30m\": %w", name, s.Idle, err)
		}
	}

	return nil
}

// IdleNever reports whether this service asked never to be auto-slept.
func (s Service) IdleNever() bool { return s.Idle == "never" || s.Idle == "0" }

// EgressDeny is the only egress value, because "allow" is the absence of the field.
const EgressDeny = "deny"

// LoadSpec reads and validates a sandbox.json.
func LoadSpec(path string) (*Spec, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		// A missing spec is the first thing anybody hits on a real repo, and `open
		// sandbox.json: no such file or directory` mentions neither the built-in templates
		// nor the flag that skips the file entirely - in a tool whose pitch is "no spec file
		// needed". The bare error is right about what happened and useless about what to do.
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("no %s here. Either start from a built-in:\n"+
				"       sbx create my-branch --template postgres     (sbx templates lists them)\n"+
				"     or write one:\n"+
				"       sbx init > %s", path, path)
		}

		return nil, err
	}

	return ParseSpec(raw, path)
}

// ServiceName is what a service may be called: the container-name rule, because that is what
// the name becomes. Exported so the CLI's own name check and this one cannot drift apart -
// they are the same rule, asserted equal by a test.
var ServiceName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

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
		// The name becomes part of a container name, so a spec that names a service something
		// the runtime will not accept cannot be created - and `sbx validate` exists to say so
		// before a commit, not after. Its own docstring sets the standard: a spec that passes
		// lint and fails create is worse than no lint at all.
		if !ServiceName.MatchString(name) {
			return nil, fmt.Errorf("%s: service name %q is not usable: start with a letter or "+
				"digit, then letters, digits, dot, dash or underscore - it becomes part of a "+
				"container name", path, name)
		}

		if err := svc.validate(name); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}

	// The sandbox-wide default gets the same check as a service's own, or a typo there is one
	// silently ignored setting rather than fourteen loud ones.
	if err := checkInterval(s.HealthInterval); err != nil {
		return nil, fmt.Errorf("%s: health_interval %w", path, err)
	}

	if err := s.checkDependencies(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	if err := s.expandEnv(osLookup); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
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
