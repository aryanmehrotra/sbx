package provider

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
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

	out, err := d.docker("info", "--format", "{{json .Runtimes}}")
	if err != nil {
		// No engine to judge, so no refusal: whatever asks next reports a dead daemon better
		// than an isolation message would, and nothing has been promised either way. The same
		// reasoning as HostsEgressFilters.
		return nil
	}

	have := dockerRuntimeNames(out)
	if slices.Contains(have, want) {
		return nil
	}

	// NO NAMES AT ALL IS NOT A NEGATIVE ANSWER. A daemon that is up always reports at least its
	// own default runtime, so an empty list means the question did not reach a daemon — the CLI
	// answered for itself. `docker info` does not reliably exit non-zero for that: against an
	// unreachable socket some versions print client-side info, warn on stderr and exit 0, which
	// is how this arm is reached without the error above firing.
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

// dockerRuntimeNames reads the runtime names out of `docker info --format {{json .Runtimes}}`.
// Unparseable output is no names, which reads as "not registered" - the safe direction, since
// the alternative is accepting a tier on the strength of output nobody could read.
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
