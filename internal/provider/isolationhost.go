package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// Whether this host can actually run a sandbox at an isolation tier, asked before anything is
// created - the same shape as FilterHoster / HostsEgressFilters next door, and for the same
// reason: the answer is a property of the HOST, so a caller is told at the create rather than
// finding out from a sandbox that came back Failed.
//
// On the command line docker enforces this by itself, because the tier reaches `docker run
// --runtime runsc`, which fails when the daemon has no such runtime. A server does not get that
// for free: it turns a request body into a create, and one that read "gvisor" and ran a plain
// container would be the silent downgrade "Isolation fails closed, and says why" forbids, with
// SECURITY.md's shared kernel underneath an agent that was promised its own.

// IsolationHost is a provider that can say, before anything is created, whether this host can
// give a sandbox a tier.
//
// Named for what the caller wants - a host that provides this tier - rather than for how docker
// decides it (a runtime registered with the daemon), and asked of the provider rather than
// probed around it (DECISIONS, "Capabilities are negotiated, not stubbed").
//
// Optional, and in the same direction as FilterHoster: it needs no sandbox, so it answers while
// a create can still be refused. A provider that does not implement it is one where the answer
// is not here to be had - kubernetes' RuntimeClass lives in the cluster, and the firecracker
// provider is a microVM per sandbox whose /dev/kvm question forFirecracker already answered when
// the provider was built, refusing by name there. Both apply the requested tier as they always
// did.
type IsolationHost interface {
	ProvidesIsolation(iso Isolation) error
}

// ProvidesIsolation implements IsolationHost for docker.
func (d *dockerProvider) ProvidesIsolation(iso Isolation) error {
	// The tier docker cannot run at all, refused in the words Create already uses for it.
	if err := dockerRefuses(iso); err != nil {
		return err
	}

	// Empty for "container", which is the daemon's own runtime: there is nothing to look for.
	// dockerRuntime is the one tier-to-runtime mapping, shared with Create, so this cannot come
	// to disagree with the runtime the container would actually be given.
	want := dockerRuntime(iso)
	if want == "" {
		return nil
	}

	have := d.registeredRuntimes()
	if slices.Contains(have, want) {
		return nil
	}

	// NO NAMES AT ALL IS NOT A NEGATIVE ANSWER. A daemon that is up always reports at least its
	// own default runtime, so an empty list means the question did not reach a daemon — the CLI
	// answered for itself. `docker info` does not reliably exit non-zero for that: against an
	// unreachable socket some versions print client-side info, warn on stderr and exit 0, so
	// registeredRuntimes cannot tell that case apart by exit status, and this is where it lands.
	//
	// Refusing here would tell an operator whose daemon is down to install a runtime they may
	// well already have — the same misdiagnosis this function exists to remove, arrived at from
	// the other side. So it is treated exactly like the unreachable-engine case above: say
	// nothing, and let whatever asks next report the dead daemon, which it does far better than
	// an isolation message could.
	if len(have) == 0 {
		return nil
	}

	// The daemon's registered runtimes are the authority, not a binary on this PATH: the daemon
	// runs the container, and where it lives in a VM this PATH means nothing. `sbx doctor` says
	// the same thing from the CLI's default engine; this asks the engine this provider is on.
	return fmt.Errorf("isolation %s needs docker's %s runtime, and this machine's daemon has "+
		"%s: run `sbx install %s` to register it and restart the daemon, or ask for no tier and "+
		"get a container. Check it with `docker info --format '{{json .Runtimes}}'`; `sbx "+
		"doctor` reports it as `isolation %s`",
		iso, want, listed(have), iso, iso)
}

// How long the daemon's runtime list is trusted, and how long the question may take.
//
// The list changes only when somebody registers a runtime and restarts the daemon, so a few
// seconds of staleness costs at most a few seconds of refusing a tier that has just been
// installed. The timeout is what makes this safe to run inside an HTTP handler at all: before
// it, a hung daemon held the create open with no deadline of its own.
const (
	runtimeAnswerTTL     = 5 * time.Second
	runtimeAnswerTimeout = 3 * time.Second
)

// runtimeAnswer is the last answer the daemon gave about its runtimes.
//
// The mutex is held ACROSS the probe, not just around the fields, so N creates arriving together
// run one `docker info` between them rather than N — the reason this exists at all is that the
// probe is a fork, in a request handler, on a path a caller can drive as fast as it likes.
type runtimeAnswer struct {
	mu    sync.Mutex
	names []string
	at    time.Time
}

// registeredRuntimes is the daemon's own list of runtimes, or none when it did not answer.
//
// None is not a negative answer — see dockerRuntimeNames — so the caller treats it as "no
// opinion" and accepts. Nothing is cached about WHICH tier was asked for: this is a property of
// the daemon, and one list answers every tier.
func (d *dockerProvider) registeredRuntimes() []string {
	d.runtimes.mu.Lock()
	defer d.runtimes.mu.Unlock()

	if !d.runtimes.at.IsZero() && time.Since(d.runtimes.at) < runtimeAnswerTTL {
		return d.runtimes.names
	}

	ctx, cancel := context.WithTimeout(context.Background(), runtimeAnswerTimeout)
	defer cancel()

	var names []string

	// A dead daemon, a hung one and a version whose output this cannot read all land here the
	// same way: no names. Each of them is "nothing has been promised either way", and whatever
	// asks next reports the daemon far better than an isolation message could — the same
	// reasoning as HostsEgressFilters.
	if out, err := d.dockerWithin(ctx, "info", "--format", "{{json .Runtimes}}"); err == nil {
		names = dockerRuntimeNames(out)
	}

	d.runtimes.names, d.runtimes.at = names, time.Now()

	return names
}

// dockerRuntimeNames reads the runtime names out of `docker info --format {{json .Runtimes}}`.
//
// Output nobody can parse is NO NAMES, and no names is "the question did not reach a daemon" —
// which ProvidesIsolation accepts rather than refuses. That is deliberate and it is not a hole:
// the tier still reaches `docker run --runtime runsc`, which the daemon itself fails when no
// such runtime is registered, so the create cannot be silently downgraded to a weaker sandbox.
// What accepting avoids is the opposite error — telling an operator whose daemon is unreachable,
// or whose docker prints something this version cannot read, to install a runtime they may
// already have.
func dockerRuntimeNames(infoJSON string) []string {
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(infoJSON), &m) != nil {
		return nil
	}

	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}

	slices.Sort(names) // a stable error message, which is also a testable one

	return names
}

func listed(names []string) string {
	if len(names) == 0 {
		return "no runtimes to report"
	}

	return "only " + strings.Join(names, ", ")
}
