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
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/aryanmehrotra/sbx/internal/egress"
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
	//	"allow" open, but through the egress filter rather than a route of its own:
	//	        HTTP and HTTPS reach anywhere, and the policy can be tightened on the
	//	        running service (`sbx egress`) without recreating it. Other protocols have
	//	        no way out, because the filter is the only door - see EgressPolicy
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

	// EgressPolicy is the general form: OpenSandbox's NetworkPolicy, verbatim - a default action
	// and ordered allow/deny rules on hosts, *.wildcards, IPs and CIDRs. Like EgressAllow it puts
	// the service behind the filter on a bridge with no route out, so every rule is enforced -
	// including a deny under a default of allow, since nothing leaves except through the filter.
	// The price of that is the same as egress: "allow": a default-allow policy opens HTTP and
	// HTTPS, not raw TCP.
	//
	// It is the policy the service STARTS with. `sbx egress` and the OpenSandbox networkpolicy
	// API change it on the running service; `sbx egress --reset` comes back to this.
	//
	// Services of one sandbox share one filter, so two that declare a policy must declare the
	// same one - the alternative is a filter enforcing a mixture nobody wrote.
	EgressPolicy *egress.Policy `json:"egress_policy,omitempty"`

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

	// CapAdd grants Linux capabilities the container would not otherwise have, by their
	// docker names without the CAP_ prefix: ["SYS_PTRACE"], ["CHECKPOINT_RESTORE"].
	//
	// It exists because some workloads genuinely cannot run without one and the failure is
	// unreadable without it. CRIU - which is what a memory checkpoint is made of - reports
	// "CRIU needs to have the CAP_SYS_ADMIN or the CAP_CHECKPOINT_RESTORE capability", and a
	// debugger inside a sandbox fails on ptrace with a permission error that names nothing.
	// Neither is a bug in sbx and neither can be worked around from inside the container.
	//
	// A list of named capabilities rather than a `privileged` flag, deliberately. Privileged
	// is not "a few more permissions": it disables seccomp and AppArmor, grants every
	// capability, and exposes the host's devices - which is a container that can reconfigure
	// the machine it is on. A spec asking for CHECKPOINT_RESTORE says what it needs and gets
	// only that, and a reviewer reading the committed file can see the difference.
	//
	// Not validated against a list of known names. Docker rejects an unknown capability at
	// create with a better message than this could paraphrase, and the set differs by kernel.
	CapAdd []string `json:"cap_add,omitempty"`

	// Entrypoint replaces the image's ENTRYPOINT: the first element is the program, the rest its
	// leading arguments, and Args follow them. Empty keeps the image's own.
	//
	// Args alone cannot express this - it only replaces CMD, which the image's ENTRYPOINT then
	// receives as arguments - and wrapping a workload in a supervisor it did not ship with is
	// exactly the case that needs the program itself replaced.
	Entrypoint []string `json:"entrypoint,omitempty"`

	// ReadOnlyVolumes mounts existing named volumes read-only: volume name -> absolute path.
	//
	// For tools sbx supplies that the image does not carry (the OpenSandbox agent, mounted at
	// /opt/sbx). Named volumes rather than bind mounts because a bind mount of a host path
	// depends on the container runtime's VM sharing that path, which on a Mac it may not - a
	// volume lives on the runtime's side of that boundary by construction.
	ReadOnlyVolumes map[string]string `json:"readonly_volumes,omitempty"`

	// VolumeMounts attaches storage the caller named - a named volume or a host directory -
	// with the options `mounts` cannot express: read-only, and a subdirectory of a volume.
	//
	// It exists for the OpenSandbox API's `volumes`, where a caller asks for exactly this and
	// the server has already decided the request is allowed (host paths only under roots the
	// operator listed, volumes only in the API's own namespace). A spec author wants `volume`
	// or `mounts` instead; this carries no policy of its own beyond "the mount is well formed".
	VolumeMounts []VolumeMount `json:"volume_mounts,omitempty"`

	// OnIdle is what going idle does: "" or "stop" stops the container (0 B, the default), and
	// "freeze" pauses it instead - memory and running processes kept, no CPU, thawed in about
	// 10 ms by the next connection.
	//
	// Freeze is the default for sandboxes created through the OpenSandbox API, whose contract
	// is that a background process started in one request is still running at the next. Here it
	// is opt-in, because it holds the memory, and holding nothing is the reason sbx exists.
	OnIdle string `json:"on_idle,omitempty"`
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
	case "", EgressDeny, EgressAllow:
	default:
		return fmt.Errorf("service %q: egress %q is not valid - it is %q, or %q for open egress "+
			"through the filter, or unset", name, s.Egress, EgressDeny, EgressAllow)
	}

	if err := s.validatePolicy(name); err != nil {
		return err
	}

	if len(s.EgressAllow) > 0 {
		for _, h := range s.EgressAllow {
			if strings.TrimSpace(h) == "" {
				return fmt.Errorf("service %q: egress_allow has a blank host", name)
			}
		}
	}

	switch s.OnIdle {
	case "", OnIdleStop, OnIdleFreeze:
	default:
		return fmt.Errorf("service %q: on_idle %q is not valid - %q or %q", name, s.OnIdle,
			OnIdleStop, OnIdleFreeze)
	}

	for vol, dest := range s.ReadOnlyVolumes {
		if strings.TrimSpace(vol) == "" || strings.ContainsAny(vol, "/:") {
			return fmt.Errorf("service %q: readonly volume %q is not a volume name - these are "+
				"named volumes, not host paths (use mounts for a host directory)", name, vol)
		}

		if !strings.HasPrefix(dest, "/") {
			return fmt.Errorf("service %q: readonly volume %q -> %q needs an absolute container path",
				name, vol, dest)
		}
	}

	for i, m := range s.VolumeMounts {
		if err := m.validate(); err != nil {
			return fmt.Errorf("service %q: volume_mounts[%d]: %w", name, i, err)
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

// VolumeMount is one entry of volume_mounts: exactly one of Volume (a named volume) or Host (an
// absolute host directory), mounted at Target.
type VolumeMount struct {
	Volume string `json:"volume,omitempty"`
	Host   string `json:"host,omitempty"`
	Target string `json:"target"`

	// SubPath mounts a directory inside the volume rather than its root. Named volumes only: a
	// host path already names whatever directory it wants.
	SubPath  string `json:"sub_path,omitempty"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

func (m VolumeMount) validate() error {
	switch {
	case (m.Volume == "") == (m.Host == ""):
		return errors.New("needs exactly one of volume (a named volume) or host (a directory)")
	case m.Volume != "" && strings.ContainsAny(m.Volume, "/:"):
		return fmt.Errorf("volume %q is not a volume name - use host for a directory", m.Volume)
	case m.Host != "" && !strings.HasPrefix(m.Host, "/"):
		return fmt.Errorf("host %q must be absolute", m.Host)
	case m.Host != "" && m.SubPath != "":
		return errors.New("sub_path is for named volumes; put the subdirectory in host instead")
	case !strings.HasPrefix(m.Target, "/"):
		return fmt.Errorf("target %q must be an absolute container path", m.Target)
	case m.SubPath != "" && (strings.HasPrefix(m.SubPath, "/") || slices.Contains(strings.Split(m.SubPath, "/"), "..")):
		return fmt.Errorf("sub_path %q must be relative and must not contain '..'", m.SubPath)
	}

	// Docker's --mount is a CSV list, so a comma inside a value would be read as the start of
	// another option - a path that silently becomes a different mount.
	for _, v := range []string{m.Volume, m.Host, m.Target, m.SubPath} {
		if strings.ContainsAny(v, ",\"\n\x00") {
			return fmt.Errorf("%q contains a comma, quote or control character, which a mount "+
				"option cannot carry", v)
		}
	}

	return nil
}

// IdleNever reports whether this service asked never to be auto-slept.
func (s Service) IdleNever() bool { return s.Idle == "never" || s.Idle == "0" }

// EgressDeny and EgressAllow are the egress values. Unset is "whatever the backend does",
// which is open with a route of its own; "allow" is open THROUGH the filter, so that it can be
// narrowed later on the running service.
const (
	EgressDeny  = "deny"
	EgressAllow = "allow"
)

// validatePolicy refuses the combinations that contradict each other. Each one is refused
// rather than resolved by a precedence rule, because every resolution silently discards half of
// what somebody wrote in a security control.
func (s Service) validatePolicy(name string) error {
	declared := 0

	for _, set := range []bool{s.Egress != "", len(s.EgressAllow) > 0, s.EgressPolicy != nil} {
		if set {
			declared++
		}
	}

	if declared > 1 {
		return fmt.Errorf("service %q: egress, egress_allow and egress_policy each say the whole "+
			"answer and they contradict - use one. An allow-list with exceptions, or open with "+
			"some hosts denied, is an egress_policy", name)
	}

	if s.EgressPolicy != nil {
		if _, err := s.EgressPolicy.Normalize(); err != nil {
			return fmt.Errorf("service %q: egress_policy: %w", name, err)
		}
	}

	return nil
}

// Filtered reports whether this service reaches the network only through the egress filter.
func (s Service) Filtered() bool {
	return len(s.EgressAllow) > 0 || s.EgressPolicy != nil || s.Egress == EgressAllow
}

// DeclaredPolicy is the egress policy the spec gives a filtered service to start with.
func (s Service) DeclaredPolicy() egress.Policy {
	switch {
	case s.EgressPolicy != nil:
		if p, err := s.EgressPolicy.Normalize(); err == nil {
			return p
		}

		// validate refuses this before anything is created; deny is the answer that cannot
		// leave anything open if a caller skipped it.
		return egress.DenyAll()
	case s.Egress == EgressAllow:
		return egress.Policy{DefaultAction: egress.ActionAllow, Egress: []egress.Rule{}}
	default:
		return egress.FromAllowList(s.EgressAllow)
	}
}

// checkEgressFilters refuses two services of one sandbox declaring different policies. They
// share one bridge and so one filter, and a CONNECT carries nothing that says which container
// opened it. Plain allow-lists are exempt: their union is what they have always meant.
func (s *Spec) checkEgressFilters() error {
	var (
		first string
		want  string
	)

	for _, name := range s.Names() {
		svc := s.Services[name]
		if !svc.Filtered() || len(svc.EgressAllow) > 0 {
			continue
		}

		h := svc.DeclaredPolicy().Hash()

		switch {
		case first == "":
			first, want = name, h
		case h != want:
			return fmt.Errorf("services %q and %q declare different egress policies, and the "+
				"services of one sandbox share one egress filter - give them the same policy, "+
				"or put them in separate sandboxes", first, name)
		}
	}

	if first == "" {
		return nil
	}

	for _, name := range s.Names() {
		if len(s.Services[name].EgressAllow) > 0 {
			return fmt.Errorf("services %q and %q share one egress filter, and one declares an "+
				"egress_allow list while the other declares a policy - write the list as "+
				"allow rules in the same egress_policy", first, name)
		}
	}

	return nil
}

// The two things going idle can mean. See Service.OnIdle.
const (
	OnIdleStop   = "stop"
	OnIdleFreeze = "freeze"
)

// Validate checks one service on its own, for callers that build a Service in code rather
// than loading a sandbox.json - the OpenSandbox API does - and want the same refusals.
func (s Service) Validate(name string) error { return s.validate(name) }

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

	if err := s.checkEgressFilters(); err != nil {
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
