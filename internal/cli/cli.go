package cli

// The commands. Everything here is provider-agnostic: it decides *what* a sandbox is and
// *when* it is serving, and asks a Provider where that lives.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aryanmehrotra/sbx/internal/daemon"
	"github.com/aryanmehrotra/sbx/internal/history"
	"github.com/aryanmehrotra/sbx/internal/logs"
	"github.com/aryanmehrotra/sbx/internal/provider"
	"github.com/aryanmehrotra/sbx/internal/spec"
)

// ── create ───────────────────────────────────────────────────────────────────

// defaultHealthTimeout is how long create waits for each service's health check when the
// caller gave no budget of its own. `sbx create` and `sbx add` have no --timeout flag; `sbx with`
// does, and passes it through createWithin.
const defaultHealthTimeout = 120 * time.Second

func Create(ctx context.Context, p provider.Provider, path, sandbox string, withOptional bool, iso provider.Isolation) error {
	return createWithin(ctx, p, path, sandbox, withOptional, iso, defaultHealthTimeout)
}

// createWithin is Create with the health-wait budget made explicit. It exists because `sbx with`
// takes a --timeout, and a create that waited its own fixed two minutes inside that budget made
// `sbx with --timeout 20s` wait 2m against a service that never answered.
func createWithin(ctx context.Context, p provider.Provider, path, sandbox string, withOptional bool, iso provider.Isolation, healthTimeout time.Duration) error {
	if err := ValidateName("sandbox", sandbox); err != nil {
		return err
	}

	sp, err := spec.LoadSpec(path)
	if err != nil {
		return err
	}

	layout, err := sp.Assign()
	if err != nil {
		return err
	}

	// Held only until the first container exists - see slotlock.go. After that the slot is
	// claimed by something every other create can see.
	releaseSlot := lockSlots()
	defer releaseSlot()

	slot, err := p.AllocSlot(ctx, sandbox)
	if err != nil {
		return err
	}

	fmt.Printf("sandbox %q  provider %s  isolation %s\n", sandbox, p.Name(), iso)

	specDir := filepath.Dir(path)

	var created []provider.Endpoint

	order, err := sp.CreationOrder()
	if err != nil {
		return err
	}

	// Asked before anything is built, because the answer is about this machine and not about
	// the spec - so `sbx validate` cannot reach it, and the per-service check finds out too
	// late. Services are created one at a time and the first failure returns without rolling
	// back, so a spec whose third service carries the allow-list used to leave the first two
	// running and the sandbox half-built, with a retry that failed in exactly the same place.
	if wantsAllowList(sp, withOptional) {
		if pf, ok := p.(provider.EgressPreflighter); ok {
			if err := pf.EgressPreflight(ctx, sandbox); err != nil {
				return err
			}
		}
	}

	shared := sharedAllowList(sp, withOptional)

	skipped := map[string]bool{}

	for _, name := range order {
		svc := sp.Services[name]

		if len(svc.EgressAllow) > 0 {
			svc.EgressAllow = shared
		}

		if svc.Optional && !withOptional {
			fmt.Printf("  %-12s skipped (optional)\n", name)

			skipped[name] = true

			continue
		}

		// A service being created that depends on one that was just skipped would be exactly
		// the failure depends_on exists to prevent, with the cause moved from "alphabetical
		// accident" to "optional accident" - and it would look like success, because the
		// ordering ran and every service that was created came up.
		for _, dep := range svc.DependsOn {
			if skipped[dep] {
				return fmt.Errorf("service %q depends on %q, which is optional and was not "+
					"created - pass --optional, or drop the dependency", name, dep)
			}
		}

		start, _ := sp.StartIndex(layout, name)

		// Resolve a build into an image first: everything downstream - the provider, the
		// labels, the wake path - only ever knows about images.
		svc, err = buildIfNeeded(ctx, p, specDir, name, svc)
		if err != nil {
			return err
		}

		// And resolve the probe interval here, where the sandbox's own default is still in
		// scope. A provider is handed one service at a time and cannot see the file it came
		// from, so leaving this to them would mean docker and kubernetes each re-deriving it
		// and eventually disagreeing about what the spec said.
		svc.HealthInterval = sp.ProbeInterval(svc).String()

		if err := createOneWithin(ctx, p, sandbox, slot, start, name, svc, specDir, iso, healthTimeout); err != nil {
			return err
		}

		// The slot now belongs to a real container, so nothing else can be handed it. Every
		// remaining service - pulls, health checks, init - proceeds unserialised.
		releaseSlot()

		created = append(created, p.Endpoints(sandbox, name, slot, start, svc.Ports)...)
	}

	fmt.Println()
	fmt.Println(readiness(sandbox, created))

	return nil
}

// readiness says what is actually true, which is not always the same sentence.
//
// The ports `sbx env` exports are the daemon's, not docker's - `sbx serve` is what accepts on
// them. Create used to print "connecting wakes it" unconditionally, and on a machine with no
// daemon that is simply false: the sandbox exists, the exports look right, and the first
// connection is refused with nothing anywhere saying why. That is the worst shape a first run
// can take, because everything reports success.
//
// So it is checked rather than asserted. `sbx ready` already refuses on the same evidence;
// this is the same question asked one step earlier, where it is a warning rather than an
// error - the sandbox really was created, and starting the daemon afterwards fixes it.
func readiness(sandbox string, eps []provider.Endpoint) string {
	var local []provider.Endpoint

	for _, e := range eps {
		// A remote docker host: the daemon fronting those ports is not this machine's to
		// check, and its absence is not something this process can conclude anything about.
		if e.Host == "127.0.0.1" {
			local = append(local, e)
		}
	}

	if len(local) == 0 {
		return "ready. Nothing needs starting again - connecting wakes it, idleness sleeps it."
	}

	// The ports are the ground truth, so they are asked first and the presence file is only
	// consulted to decide WHICH message when they are dead.
	//
	// Dialling first because a refused connection is the fact, and the record is only ever
	// evidence about the cause. One daemon serves the whole machine - `sbx serve` refuses to
	// start a second - so the record is now reliable; asking the port anyway costs nothing
	// and means a stale or unwritable record degrades the advice rather than the answer.
	if waitReachable(local, 0) {
		return "ready. Nothing needs starting again - connecting wakes it, idleness sleeps it."
	}

	// Two different problems that look identical from a refused connection, and they need
	// opposite advice: start a daemon, versus wait a moment for the one you have.
	// Asked for THIS sandbox: a daemon started with --only fronts the names in its scope, and is
	// the daemon to wait for when this one is among them.
	if _, ok := daemon.Serving(sandbox); !ok {
		return "no `sbx serve` is running, so nothing accepts on the ports `sbx env` exports.\n" +
			"Start one - once per machine, not once per sandbox:\n\n" +
			"    sbx serve --idle 5m &\n\n" +
			"deploy/ has a launchd plist and a systemd unit for running it supervised."
	}

	// There is a daemon, and it finds new sandboxes on its refresh tick - so for a moment
	// after create, the exported ports are still dead. Waiting here rather than handing back
	// an address that is about to work is the difference between the README's three-line
	// quickstart being true and being true-eventually.
	if waitReachable(local, pickupWait) {
		return "ready. Nothing needs starting again - connecting wakes it, idleness sleeps it."
	}

	return "created, but the running `sbx serve` has not picked it up yet.\n" +
		"It looks for new sandboxes on its --refresh interval; give it one, or restart it."
}

// pickupWait is how long create waits for a running daemon to bind a new sandbox's ports.
var pickupWait = 30 * time.Second

// waitReachable blocks until every endpoint accepts, or the deadline passes.
func waitReachable(eps []provider.Endpoint, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)

	for _, e := range eps {
		for !daemon.Reachable(e.Port) {
			if time.Now().After(deadline) {
				return false
			}

			time.Sleep(200 * time.Millisecond)
		}
	}

	return true
}

func createOne(ctx context.Context, p provider.Provider, sandbox string, slot, start int,
	name string, svc spec.Service, specDir string, iso provider.Isolation,
) error {
	return createOneWithin(ctx, p, sandbox, slot, start, name, svc, specDir, iso, defaultHealthTimeout)
}

func createOneWithin(ctx context.Context, p provider.Provider, sandbox string, slot, start int,
	name string, svc spec.Service, specDir string, iso provider.Isolation, healthTimeout time.Duration,
) error {
	eps := p.Endpoints(sandbox, name, slot, start, svc.Ports)

	// Read before Create, not after: whether this create made the service is what matters, and
	// Running cannot say. A microVM's Create ends by putting the new VM to sleep, so a service
	// made a moment ago lists as not running too - and it still needs its health wait and init.
	prior, lookupErr := unitFor(ctx, p, sandbox, name)
	asleepBefore := lookupErr == nil && !prior.Running

	if err := p.Create(ctx, sandbox, slot, start, name, svc, eps, specDir, iso); err != nil {
		return fmt.Errorf("service %q: %w", name, err)
	}

	ref, err := refFor(ctx, p, sandbox, name)
	if err != nil {
		return err
	}

	// A service that already existed and is asleep is left as it is. Its mount check, health wait
	// and init all exec into it, and an exec into a stopped container fails - which the file check
	// used to report as "your file mounted as a directory". They ran when it was created. Waking
	// it here to repeat them would start a container behind the daemon's back, so this says how
	// to re-run them instead.
	if asleepBefore {
		fmt.Printf("  %-12s asleep - left as it is; to re-run its checks and init: sbx wake %s, then this create again\n",
			name, sandbox)
		return nil
	}

	if err := checkMounts(ctx, p, ref, name, svc, specDir); err != nil {
		return fmt.Errorf("%w\n%s", err, discardBrokenMount(ctx, p, sandbox, ref))
	}

	if svc.Health != "" {
		if err := waitHealthy(ctx, p, ref, svc.Health, healthTimeout); err != nil {
			return fmt.Errorf("service %q: %w", name, err)
		}
	}

	// Init runs once, after the service first reports healthy. Not on every start: a woken
	// sandbox already has whatever this created.
	for _, step := range svc.Init {
		// Init steps are written as shell one-liners in the spec, so they ask for a shell.
		if _, err := p.Exec(ctx, ref, []string{"sh", "-c", step}); err != nil {
			return fmt.Errorf("service %q: init step failed: %w", name, err)
		}
	}

	fmt.Printf("  %-12s ✓ %s\n", name, joinEndpoints(eps))

	return nil
}

// discardBrokenMount takes the container whose mount check failed out of service, and says how.
//
// A mount is fixed when the container is created, so this container is broken by construction:
// every start mounts the same wrong path. It used to be left up and awake after the error,
// serving with the broken mount, and a re-run after fixing the path found it "already exists" and
// kept it. The rest of the sandbox is deliberately kept - re-running create is how a half-built
// sandbox is finished, and rolling everything back would throw away services that are fine.
//
// A backend that cannot remove one service's workload stops it instead. That keeps it from
// serving, but a re-run leaves a stopped service as it is, so the message says to remove the
// sandbox rather than to re-run.
func discardBrokenMount(ctx context.Context, p provider.Provider, sandbox, ref string) string {
	if r, ok := p.(provider.UnitRemover); ok {
		if err := r.RemoveUnit(ctx, ref); err == nil {
			return fmt.Sprintf("     Its container was removed (removed %s): every start would mount the same wrong path.\n"+
				"     The rest of the sandbox is kept. Fix the path, then re-run the same sbx create %s to finish it.",
				ref, sandbox)
		}
	}

	if err := p.Stop(ctx, ref); err != nil {
		return fmt.Sprintf("     Its container %s could not be removed or stopped (%v), so it may still be serving with\n"+
			"     the broken mount. Fix the path, then: sbx rm %s, and create it again.", ref, err, sandbox)
	}

	return fmt.Sprintf("     Its container was stopped (stopped %s) so it does not serve with the broken mount. This\n"+
		"     backend cannot remove one service, so fix the path, then: sbx rm %s, and create it again.",
		ref, sandbox)
}

// checkMounts asserts that every declared file arrived as a file.
//
// A bind mount whose source the container runtime cannot reach does not fail - docker
// creates an empty directory at the destination. Anything that then reads that path gets a
// directory, and the resulting error talks about config parsing or a missing file rather
// than about a mount. This turns the most expensive silent failure in the project into one
// line naming the path.
func checkMounts(ctx context.Context, p provider.Provider, ref, name string, svc spec.Service, specDir string) error {
	for host, dest := range svc.Mounts {
		if err := checkOneMount(ctx, p, ref, name, host, dest, specDir); err != nil {
			return err
		}
	}

	for host, dest := range svc.Files {
		if _, err := p.Exec(ctx, ref, []string{"test", "-f", dest}); err != nil {
			return fmt.Errorf(
				"service %q: %s did not mount as a file - the container has a directory at %s.\n"+
					"The runtime could not reach %s, so it created an empty one. A VM-backed docker "+
					"only shares some host paths; move the file somewhere it can see, such as under "+
					"your home directory",
				name, host, dest, host)
		}
	}

	return nil
}

// checkOneMount proves a bind mount reaches the host directory it names.
//
// A directory mount cannot be checked the way a file can: the failure produces an empty
// directory at the destination, and an empty directory is indistinguishable from a real one
// that happens to be empty. So this writes a marker on the host and looks for it inside. It is
// two syscalls and an exec, and it turns the most expensive silent failure in the project -
// measured again while adding this feature - into one line naming the path.
func checkOneMount(ctx context.Context, p provider.Provider, ref, name, host, dest, specDir string) error {
	abs := host
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(specDir, host)
	}

	// Absolute, so the error names a place the reader can go and look at. `--spec sandbox.json`
	// makes specDir "." and Join cleans "./shared" back to "shared", which is the one path in
	// the message that has to be unambiguous.
	if a, err := filepath.Abs(abs); err == nil {
		abs = a
	}

	marker := ".sbx-mount-check"

	if err := os.WriteFile(filepath.Join(abs, marker), []byte("sbx"), 0o644); err != nil {
		return fmt.Errorf("service %q: could not write to %s, which it mounts at %s: %w",
			name, abs, dest, err)
	}

	defer func() { _ = os.Remove(filepath.Join(abs, marker)) }()

	if _, err := p.Exec(ctx, ref, []string{"test", "-e", dest + "/" + marker}); err != nil {
		return fmt.Errorf(
			"service %q: %s did not mount - the container has an empty directory at %s.\n"+
				"The runtime could not reach %s, so it created one. A VM-backed docker only "+
				"shares some host paths; move it somewhere the VM can see, such as under your "+
				"home directory, or add the path to Docker Desktop's file sharing",
			name, host, dest, abs)
	}

	return nil
}

func refFor(ctx context.Context, p provider.Provider, sandbox, service string) (string, error) {
	u, err := unitFor(ctx, p, sandbox, service)
	return u.Ref, err
}

func unitFor(ctx context.Context, p provider.Provider, sandbox, service string) (provider.Unit, error) {
	units, err := p.List(ctx, sandbox)
	if err != nil {
		return provider.Unit{}, err
	}

	for _, u := range units {
		if u.Service == service {
			return u, nil
		}
	}

	return provider.Unit{}, fmt.Errorf("service %q was created but the provider does not list it", service)
}

// waitHealthy blocks until the workload says it is serving, or gives up loudly.
//
// Returning nil on timeout would be the expensive kind of wrong: everything downstream
// would report a clean run against a database that never came up.
// It asks Probe, not Healthy, for the same reason the wake path does: the platform
// republishes health on its own interval and that lag was 98% of the time spent here.
func waitHealthy(ctx context.Context, p provider.Provider, ref, command string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	checked := false

	for time.Now().Before(deadline) {
		if serving, declared := p.Probe(ctx, ref); serving {
			return nil
		} else if !declared {
			// Nothing to ask. Say so rather than spin until the deadline pretending to check.
			return nil
		}

		// The first failure is the one worth looking at closely. A health command that is not
		// in the image fails identically to one that is merely early - and waiting two
		// minutes to say "never became ready" for a missing binary is the least useful
		// message this tool can produce. The shell distinguishes them for us: 127 is "not
		// found" and 126 is "found but not executable", and neither improves with time.
		if !checked && command != "" {
			checked = true

			if why, fatal := healthWillNeverPass(ctx, p, ref, command); fatal {
				return fmt.Errorf("the health command %q cannot run in this image: %s\n"+
					"     The command has to exist inside the container. Check with:\n"+
					"       docker run --rm --entrypoint sh <image> -c 'command -v <tool>'",
					command, why)
			}
		}

		time.Sleep(100 * time.Millisecond)
	}

	// Two minutes of nothing, so say what the check actually returned rather than only that
	// it kept returning it - and what the WORKLOAD said, which is usually the actual answer.
	//
	// A container that died on startup fails the health check for the whole timeout and then
	// gets reported as a health-check problem, which is the wrong subject: the check is fine
	// and there is nothing to check. Measured with k3s in a sandbox - it exited in under a
	// second with "failed to evacuate root cgroup: mkdir /sys/fs/cgroup/init: read-only file
	// system", and sbx spent two minutes to say the health command returned "no output". The
	// reason was in the log the whole time and it took a `docker logs` to find.
	last := lastLines(ctx, p, ref, 5)

	if why := runtimeState(ctx, p, ref); why != "" {
		last += "\n     " + why
	}

	if command != "" {
		if out, err := p.Exec(ctx, ref, []string{"sh", "-c", command}); err != nil {
			return fmt.Errorf("%s never became ready within %s - the health command %q still "+
				"fails: %s%s", ref, timeout, command, firstLine(out), last)
		}
	}

	return fmt.Errorf("%s never became ready within %s%s", ref, timeout, last)
}

// runtimeState is the runtime's own account of a workload that never served, as one clause for
// an error, or "" when it is running or the backend cannot say.
//
// Probe answers only serving or not, so a wait that timed out cannot tell "the check kept
// failing" from "the container exited" or "the engine never answered" - and those send the
// reader to three different places. The last one matters most: an engine stalled during a Kata
// start fails every inspect, and "never became ready" alone blames the workload.
func runtimeState(ctx context.Context, p provider.Provider, ref string) string {
	er, ok := p.(provider.ExitReporter)
	if !ok {
		return ""
	}

	st, err := er.ExitOf(ctx, ref)
	if err != nil {
		return "the runtime could not be asked about it: " + err.Error()
	}

	if st.Status == "running" {
		return ""
	}

	return "its container is not running: " + st.String()
}

// lastLines is what the workload printed, formatted for the end of an error, or "" if it said
// nothing or could not be asked.
//
// Best effort on purpose: this runs on a path that is already failing, and a provider that
// cannot produce logs should not turn one error into a different one.
func lastLines(ctx context.Context, p provider.Provider, ref string, n int) string {
	var b strings.Builder

	if err := p.Logs(ctx, ref, n, false, &b); err != nil {
		return ""
	}

	out := strings.TrimSpace(b.String())
	if out == "" {
		return ""
	}

	var sb strings.Builder

	sb.WriteString("\n     what it said before giving up:")

	for line := range strings.SplitSeq(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			sb.WriteString("\n       " + line)
		}
	}

	return sb.String()
}

// healthWillNeverPass reports whether the health command failed in a way that waiting cannot
// fix, and why.
func healthWillNeverPass(ctx context.Context, p provider.Provider, ref, command string) (string, bool) {
	out, err := p.Exec(ctx, ref, []string{"sh", "-c", command})
	if err == nil {
		return "", false
	}

	// The provider folds the command's output into its error rather than returning it, so
	// the reason is in whichever of the two actually has it.
	text := out + " " + err.Error()
	detail := firstLine(out)

	if detail == "no output" {
		detail = shellReason(err.Error())
	}

	// A container that has not started yet fails every exec, and none of those failures say
	// anything about the image. Waiting is precisely what fixes them, which makes them the
	// opposite of what this function looks for - so they are excluded before anything else.
	//
	// This is what kubernetes does on a normal create: sbx probes as soon as the Deployment
	// exists, the pod is still scheduling, and the apiserver answers `container not found
	// ("app")`. That contains "not found", which the check below used to match - so `sbx
	// create --provider kubernetes` reported "the health command cannot run in this image"
	// and told the reader to go and check whether the tool was installed. Measured against
	// nginx:alpine: wget is at /usr/bin/wget and the exact command exits 0 a moment later.
	// A wrong diagnosis that sends somebody to fix the wrong thing is worse than no
	// diagnosis, and this one rejected a spec that was correct.
	for _, transient := range []string{
		"container not found",          // kubernetes, pod not started
		"unable to upgrade connection", // kubernetes, exec before the container is up
		"ContainerCreating",
		"PodInitializing",
		"is not running",    // docker
		"No such container", // docker, between create and start
	} {
		if strings.Contains(text, transient) {
			return "", false
		}
	}

	switch {
	// The exit status is the authoritative signal and comes from the shell itself: 127 is
	// "not found", 126 is "found but not executable", and neither improves with time.
	case strings.Contains(text, "exit status 127"), strings.Contains(text, "exit status 126"):
		return detail, true

	// The shell's own wording, kept narrow. A shell says `sh: wget: not found`, with the
	// colon - which is what distinguishes it from a runtime talking about a container.
	case strings.Contains(text, ": not found"), strings.Contains(text, "Permission denied"):
		return detail, true
	}

	return "", false
}

// shellReason pulls the shell's own complaint out of a wrapped exec error.
//
// The provider's error is the whole command line plus the exit status plus the output, which
// is the right thing to have and the wrong thing to show. What a reader needs is the last
// line: "sh: pg_isready: not found".
func shellReason(s string) string {
	s = strings.TrimSpace(s)

	// Everything after the exit status is the command's own output. Before it is the command
	// line sbx built, which the reader did not type and does not need.
	if i := strings.Index(s, "exit status "); i >= 0 {
		rest := s[i+len("exit status "):]
		if j := strings.Index(rest, ": "); j >= 0 {
			s = strings.TrimSpace(rest[j+2:])
		}
	}

	return firstLine(s)
}

// firstLine keeps an error readable: a failing command's first line is the reason, and the
// rest is usually a usage message nobody asked for.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}

	if len(s) > 200 {
		s = s[:200] + "..."
	}

	if s == "" {
		return "no output"
	}

	return s
}

func joinEndpoints(eps []provider.Endpoint) string {
	parts := make([]string, 0, len(eps))
	for _, e := range eps {
		parts = append(parts, e.String())
	}

	return strings.Join(parts, " ")
}

// ── add ──────────────────────────────────────────────────────────────────────

// cmdAdd puts a service nobody declared into an existing sandbox.
//
// This is the affordance an agent needs. A spec covers what a repo always wants; an agent
// mid-task wants a Postgres to try a migration against, and should be able to have one
// inside its own sandbox - addressed, sleeping when idle, destroyed with the sandbox -
// rather than reaching for a stray container that outlives the task and belongs to nobody.
func Add(ctx context.Context, p provider.Provider, specPath, sandbox, name, image string,
	containerPorts []int, health string, env map[string]string, volume string, args []string, iso provider.Isolation,
) error {
	// The service name becomes a container name, exactly as a sandbox name does. This is the
	// agent-facing path, so it is also the one most likely to be handed something shaped like
	// a branch or a task id - and without this it fails at docker, late, with a dump of the
	// whole command instead of the name that was wrong.
	if err := ValidateName("service", name); err != nil {
		return err
	}

	units, err := p.List(ctx, sandbox)
	if err != nil {
		return err
	}

	if len(units) == 0 {
		return UnknownSandbox(ctx, p, sandbox)
	}

	for _, u := range units {
		if u.Service == name {
			return fmt.Errorf("sandbox %q already has a service called %q", sandbox, name)
		}
	}

	start, err := freeIndex(specPath, units, len(containerPorts))
	if err != nil {
		return fmt.Errorf("sandbox %q: %w", sandbox, err)
	}

	svc := spec.Service{Image: image, Ports: containerPorts, Health: health, Env: env, Volume: volume, Args: args}

	return createOne(ctx, p, sandbox, units[0].Slot, start, name, svc, filepath.Dir(specPath), iso)
}

// freeIndex finds room for n consecutive ordinals that are neither taken nor reserved.
//
// Reserved matters as much as taken: the spec assigns an ordinal to every declared service
// including optional ones nobody created, so an ad-hoc service that ignored those would sit
// where ClickHouse is going to want to be the first time somebody passes --optional.
func freeIndex(specPath string, units []provider.Unit, n int) (int, error) {
	used := map[int]bool{}

	// Unexpanded: only the layout is read here, and an unset secret made this whole lookup
	// fail - silently, so the reserved ordinals were ignored.
	if sp, err := spec.LoadSpecUnexpanded(specPath); err == nil {
		layout, err := sp.Assign()
		if err != nil {
			return 0, err
		}

		for _, a := range layout {
			used[a.Index] = true
		}
	}

	for _, u := range units {
		for i := range u.Client {
			used[u.Index+i] = true
		}
	}

	for i := range spec.MaxOrdinals {
		ok := true

		for j := range n {
			if used[i+j] {
				ok = false
				break
			}
		}

		if ok && i+n <= spec.MaxOrdinals {
			return i, nil
		}
	}

	return 0, fmt.Errorf("no run of %d free ordinals left", n)
}

// ── env ──────────────────────────────────────────────────────────────────────

// cmdEnv prints the exports a repo's existing tooling already reads.
//
// Both a host and a port, always. Locally the host is loopback and only the port carries
// information, but a caller that hardcodes localhost is a caller that cannot be deployed -
// and the whole point of the provider seam is that the same spec works in both places.
// shellFormat renders one variable the way the caller's shell will accept it.
//
// `export FOO=bar` is not a universal fact, it is one shell family's syntax. A tool that
// only ever emits it works on the author's laptop and silently produces garbage in
// PowerShell, which is exactly the kind of assumption that makes something "cross-platform
// except in practice".
func shellFormat(shell, key, value string) (string, error) {
	switch shell {
	case "", "posix", "sh", "bash", "zsh":
		return fmt.Sprintf("export %s=%s", key, shellQuote(value)), nil
	case "fish":
		return fmt.Sprintf("set -gx %s %s", key, shellQuote(value)), nil
	case "powershell", "pwsh":
		return fmt.Sprintf("$env:%s = %s", key, powershellQuote(value)), nil
	case "cmd":
		return fmt.Sprintf("set %s=%s", key, value), nil
	default:
		return "", fmt.Errorf("unknown shell %q (want posix, fish, powershell, cmd or json)", shell)
	}
}

// shellQuote is single-quote escaping, which is total: inside single quotes a POSIX shell
// interprets nothing, and the only character needing care is the quote itself.
func shellQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

func powershellQuote(v string) string {
	return `"` + strings.ReplaceAll(v, `"`, `""`) + `"`
}

// detectShell guesses from the environment, so the common case needs no flag.
func detectShell() string {
	if os.Getenv("PSModulePath") != "" && os.Getenv("SHELL") == "" {
		return "powershell"
	}

	if sh := os.Getenv("SHELL"); sh != "" {
		return filepath.Base(sh)
	}

	return "posix"
}

// stderr is where a command's notes go - a variable name `sbx env` could not hand out, a followed
// service that went to sleep - so a test can read them. Never stdout: `eval "$(sbx env)"` and
// `--shell json` parse stdout, and a note there breaks them.
var stderr io.Writer = os.Stderr

// envVars resolves a sandbox's exports into ordered KEY,VALUE pairs. Env formats them for a
// shell; With injects them into a child process. One resolver, so a scoped run and an `eval`
// see exactly the same variables.
func envVars(ctx context.Context, p provider.Provider, path, sandbox string) ([][2]string, error) {
	// The sandbox is checked first, on purpose. Loading the spec first meant that `sbx env
	// typo-x` in a directory with no sandbox.json reported the missing spec - so the reader
	// was told to write a spec when what they had actually done was mistype a name.
	units, err := p.List(ctx, sandbox)
	if err != nil {
		return nil, err
	}

	if len(units) == 0 {
		return nil, UnknownSandbox(ctx, p, sandbox)
	}

	// Unexpanded: printing ports needs no secret. Create already refused an unset one.
	sp, err := spec.LoadSpecUnexpanded(path)
	if err != nil {
		return nil, err
	}

	layout, err := sp.Assign()
	if err != nil {
		return nil, err
	}

	slot := units[0].Slot
	index := map[string]provider.Endpoint{}

	for _, name := range sp.Names() {
		svc := sp.Services[name]
		start, _ := sp.StartIndex(layout, name)

		for i, e := range p.Endpoints(sandbox, name, slot, start, svc.Ports) {
			index[fmt.Sprintf("%s:%d", name, svc.Ports[i])] = e
		}
	}

	vars := [][2]string{
		{"SBX_SANDBOX", sandbox},
		{"SBX_PROVIDER", p.Name()},
	}

	for _, env := range provider.SortedKeys(sp.Exports) {
		ep, ok := index[sp.Exports[env]]
		if !ok {
			return nil, fmt.Errorf("export %s: %s is not assigned an endpoint", env, sp.Exports[env])
		}

		if host, ok := hostVar(env); ok {
			vars = append(vars, [2]string{host, ep.Host})
		}

		vars = append(vars, [2]string{env, strconv.Itoa(ep.Port)})
	}

	return append(vars, unexportedVars(sp, units, index, vars)...), nil
}

// unexportedVars addresses every service no export names: one added with `sbx add`, which is
// not in sandbox.json and so cannot have an export, or a spec service nobody exported. Without
// this the only way to find its port was `sbx list` and a copy-paste. Each gets
// <SERVICE>_HOST and <SERVICE>_PORT for its first port - asleep or not, since connecting wakes
// it. A derived name never replaces one already set: the spec author's export is the contract,
// and a service called "database" must not move DATABASE_PORT.
// Two services deriving the same name get neither, and every name withheld is warned about.
func unexportedVars(sp *spec.Spec, units []provider.Unit, index map[string]provider.Endpoint, have [][2]string) [][2]string {
	taken := map[string]bool{}
	for _, kv := range have {
		taken[kv[0]] = true
	}

	exported := map[string]bool{}
	for _, ref := range sp.Exports {
		svc, _, _ := strings.Cut(ref, ":")
		exported[svc] = true
	}

	sorted := slices.Clone(units)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Service < sorted[j].Service })

	type candidate struct {
		service string
		ep      provider.Endpoint
	}

	// Grouped by the name each would take before any is handed out, because a collision has
	// to be seen whole: taking them in order gave the name to whichever service sorted first.
	byName := map[string][]candidate{}

	var names []string

	for _, u := range sorted {
		if exported[u.Service] {
			continue
		}

		ep, ok := provider.Endpoint{}, false
		if svc, inSpec := sp.Services[u.Service]; inSpec && len(svc.Ports) > 0 {
			ep, ok = index[fmt.Sprintf("%s:%d", u.Service, svc.Ports[0])]
		}

		if !ok && len(u.Client) > 0 {
			ep, ok = u.Client[0], true
		}

		if !ok {
			continue // no ports: nothing to connect to
		}

		base := envName(u.Service)
		if len(byName[base]) > 0 && byName[base][len(byName[base])-1].service == u.Service {
			continue // one service listed twice is not a collision with itself
		}

		if byName[base] == nil {
			names = append(names, base)
		}

		byName[base] = append(byName[base], candidate{u.Service, ep})
	}

	var out [][2]string

	// Every case that hands a name to nobody says so, on stderr (see the stderr var). Silence was
	// the bug: the service had no variables, and the first sign was a client dialling another.
	warn := func(format string, args ...any) {
		fmt.Fprintf(stderr, "sbx: warning: "+format+"\n", args...)
	}

	for _, base := range names {
		cs := byName[base]
		host, port := base+"_HOST", base+"_PORT"

		switch {
		case taken[host] || taken[port]:
			// An export always wins: it is what the spec author wrote down, and a service
			// called "database" must not move DATABASE_PORT.
			held := port
			if !taken[port] {
				held = host
			}

			for _, c := range cs {
				warn("service %q gets no %s: an export already has that name. Give it an "+
					"`exports` entry of its own in sandbox.json, or rename the service", c.service, held)
			}

		case len(cs) > 1:
			// None of them gets it. Handing it to one means the name points at the wrong
			// service for anyone who meant another, and nothing would say which.
			quoted := make([]string, len(cs))
			for i, c := range cs {
				quoted[i] = fmt.Sprintf("%q", c.service)
			}

			warn("services %s all map to %s and %s, so none of them gets those. Give each an "+
				"`exports` entry in sandbox.json, or rename one", strings.Join(quoted, " and "), host, port)

		default:
			taken[host], taken[port] = true, true
			out = append(out, [2]string{host, cs[0].ep.Host}, [2]string{port, strconv.Itoa(cs[0].ep.Port)})
		}
	}

	return out
}

// envName turns a service name into a variable name: upper case, anything not a letter or digit
// becomes _, and a leading digit gets a _ in front, because no shell accepts a variable that
// starts with one.
func envName(service string) string {
	b := []byte(strings.ToUpper(service))
	for i, c := range b {
		if (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			b[i] = '_'
		}
	}

	if len(b) > 0 && b[0] >= '0' && b[0] <= '9' {
		return "_" + string(b)
	}

	return string(b)
}

func Env(ctx context.Context, p provider.Provider, path, sandbox, shell string) error {
	vars, err := envVars(ctx, p, path, sandbox)
	if err != nil {
		return err
	}

	if shell == "" {
		shell = detectShell()
	}

	// JSON is not a shell but it is how an agent or a script should read this: no quoting
	// rules to get wrong, and no eval.
	if shell == "json" {
		out := map[string]string{}
		for _, kv := range vars {
			out[kv[0]] = kv[1]
		}

		body, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}

		fmt.Println(string(body))

		return nil
	}

	for _, kv := range vars {
		line, err := shellFormat(shell, kv[0], kv[1])
		if err != nil {
			return err
		}

		fmt.Println(line)
	}

	return nil
}

// ── ready ────────────────────────────────────────────────────────────────────

// cmdReady is what a build harness calls: it wakes the sandbox by asking for it, then
// blocks until every service reports serving, and fails loudly if one never does.
//
// This is the whole reason a harness needs no "up" command. Waiting is enough, because
// asking is what starts things.
func Ready(ctx context.Context, p provider.Provider, sandbox string, timeout time.Duration) error {
	units, err := p.List(ctx, sandbox)
	if err != nil {
		return err
	}

	if len(units) == 0 {
		return UnknownSandbox(ctx, p, sandbox)
	}

	var unverifiable []string

	deadline := time.Now().Add(timeout)

	for _, u := range units {
		// Locally, connecting is the wake signal and the daemon owns the port. Elsewhere
		// there is no daemon in front, so ask the provider directly. Both are "make this
		// serve", neither is a lifecycle command anyone else may issue.
		if isLocal(u) {
			for _, e := range u.Client {
				daemon.Knock(e.Port)
			}
		} else if !u.Running {
			if err := p.Start(ctx, u.Ref); err != nil {
				return fmt.Errorf("%s: %w", u.Ref, err)
			}
		}
	}

	for _, u := range units {
		if _, declared := p.Healthy(ctx, u.Ref); !declared {
			// Not skipped silently. This function exists to be believed, and a service it
			// could not check is not a service it may vouch for.
			unverifiable = append(unverifiable, u.Service)
			continue
		}

		// No health command in hand here - Ready works from what the provider reports, not
		// from a spec - so the fast-fail check is skipped and this behaves as it always did.
		if err := waitHealthy(ctx, p, u.Ref, "", timeout); err != nil {
			return err
		}

		fmt.Printf("  %-24s serving\n", u.Service)
	}

	// A service with no health check has nothing to wait on, and one whose engine could not be
	// asked used to look the same - so this printed "serving" for a container that had exited.
	// Whatever the checks said, a workload that is not running is not serving.
	//
	// A service with a health check that passed was running when it passed. One without has only
	// its state to go on, and a container that exits on startup is "running" for the moment after
	// its start - which is when this first looks. So it has to stay running for a settle window,
	// the same two seconds the daemon gives an unverified wake before calling it awake. Found
	// live: redis with a bad flag and no health check passed a single look.
	settle := time.Duration(0)
	if len(unverifiable) > 0 {
		settle = unverifiedSettle
	}

	if err := waitRunning(ctx, p, sandbox, deadline, settle); err != nil {
		return err
	}

	// Running and healthy is still not serving. The health check runs INSIDE the container, so it
	// passes on a workload no host connection can reach - see waitWorkloads.
	if err := waitWorkloads(ctx, sandbox, workloadDials(p, units), deadline); err != nil {
		return err
	}

	if len(unverifiable) > 0 {
		fmt.Fprintf(os.Stderr,
			"sbx: warning: %s declare no health check, so nothing here checked whether they\n"+
				"     are serving - only that they exist. Give them a health command.\n",
			strings.Join(unverifiable, ", "))
	}

	// The services are healthy. That is not the same as the sandbox being usable, and the
	// difference is what a caller trips over: `sbx env` hands out the PUBLIC port, and only
	// the daemon answers on it. Without one running, this used to print "is serving" while
	// the address it had just exported accepted nothing - a green light on a dead address,
	// which is worse than a red one.
	var unreachable []string

	for _, u := range units {
		if !isLocal(u) {
			continue
		}

		for _, e := range u.Client {
			if !daemon.Reachable(e.Port) {
				unreachable = append(unreachable, fmt.Sprintf("%s (:%d)", u.Service, e.Port))
			}
		}
	}

	if len(unreachable) > 0 {
		return fmt.Errorf("%s serving, but nothing answers on %s - those are the ports\n"+
			"     `sbx env` exports, and `sbx serve` is what fronts them. Start the daemon,\n"+
			"     or connect to the backing ports directly if you do not want one",
			sandbox, strings.Join(unreachable, ", "))
	}

	fmt.Printf("sandbox %q is serving\n", sandbox)

	return nil
}

// unverifiedSettle is how long a service with no health check must stay running before ready
// believes it. The daemon waits the same before marking such a wake awake (proxy.go).
const unverifiedSettle = 2 * time.Second

// waitRunning blocks until every unit of the sandbox has been running for a continuous settle
// window, and names each one that is not when the deadline passes. It always looks at least
// once, so a deadline the health waits used up still gets an answer rather than a pass.
//
// It polls because the wake it follows is asynchronous: a knock returns when the daemon accepts,
// not when the container is up. And it wants a window, not a look or two, because a container
// that exits on startup is running for a moment after every start - and with a daemon in front
// it is started again and again, so two looks a settle apart can both land on one. Measured
// live: one `sbx ready` in three passed that way before this polled through the window.
func waitRunning(ctx context.Context, p provider.Provider, sandbox string, deadline time.Time, settle time.Duration) error {
	var upSince time.Time // when every unit was first seen running in this unbroken stretch

	for {
		units, err := p.List(ctx, sandbox)
		if err != nil {
			return err
		}

		var down []provider.Unit

		for _, u := range units {
			if !u.Running {
				down = append(down, u)
			}
		}

		if len(down) == 0 {
			if upSince.IsZero() {
				upSince = time.Now()
			}

			if time.Since(upSince) >= settle {
				return nil
			}
		} else {
			upSince = time.Time{}

			if !time.Now().Before(deadline) {
				var b strings.Builder

				for _, u := range down {
					fmt.Fprintf(&b, "\n     %s is not running", u.Service)

					if why := runtimeState(ctx, p, u.Ref); why != "" {
						b.WriteString("\n       " + why)
					}

					fmt.Fprintf(&b, "\n       see why: sbx logs %s %s", sandbox, u.Service)
				}

				return fmt.Errorf("sandbox %q is not serving:%s", sandbox, b.String())
			}
		}

		time.Sleep(100 * time.Millisecond)
	}
}

// Sleep parks a sandbox now: it stops every running service, dropping each to 0 B of memory
// without waiting out the idle timer. This is an explicit override of the idle policy, not a
// second owner of the lifecycle - the daemon still wakes them on the next connection, exactly
// as it would have. It is the pair to Ready: one command to stand a sandbox down, one to bring
// it up, for an orchestrator that wants to park a sandbox rather than wait for it to go quiet.
func Sleep(ctx context.Context, p provider.Provider, sandbox string) error {
	units, err := p.List(ctx, sandbox)
	if err != nil {
		return err
	}

	if len(units) == 0 {
		return UnknownSandbox(ctx, p, sandbox)
	}

	slept := 0

	// Layer by layer, each layer in parallel. One at a time, every stop waited out docker's
	// 10 s grace in turn: four alpine services took 30 s, and the 14-service zopnight stack
	// could take over two minutes to do something that is meant to be instant.
	for _, layer := range sleepLayers(units) {
		var (
			wg   sync.WaitGroup
			errs = make([]error, len(layer))
		)

		for i, u := range layer {
			wg.Go(func() { errs[i] = sleepOne(ctx, p, sandbox, u) })
		}

		wg.Wait()

		// Printed after the layer, in its order, so the output does not depend on which
		// container happened to exit first.
		for i, u := range layer {
			if errs[i] == nil {
				fmt.Printf("  %-24s slept\n", u.Service)
				slept++
			}
		}

		// A failed layer ends it: the next one is what this one depends on, and stopping a
		// database under an app that would not stop is the order this exists to avoid.
		if err := errors.Join(errs...); err != nil {
			return err
		}
	}

	if slept == 0 {
		fmt.Printf("sandbox %q is already asleep\n", sandbox)

		return nil
	}

	fmt.Printf("sandbox %q asleep - %d service(s) at 0 B\n", sandbox, slept)

	return nil
}

// sleepOne stops one service, thawing it first if it is frozen, and journals the outcome.
//
// A frozen service is not running, and `sbx sleep` used to skip it as "already asleep" - while
// it held every byte of its memory, the one state where sleeping is the whole point. It is
// thawed first because a stop signal sent to a frozen process is only queued: docker waits out
// the whole grace period and then kills it.
func sleepOne(ctx context.Context, p provider.Provider, sandbox string, u provider.Unit) error {
	fail := func(err error) error {
		journalEvent(sandbox, u.Service, "sleepFailed", 0, err, "could not sleep: `sbx sleep`")

		return fmt.Errorf("%s: %w", u.Ref, err)
	}

	if u.Paused {
		if pa, ok := p.(provider.Pauser); ok {
			if err := pa.Unpause(ctx, u.Ref); err != nil {
				return fail(err)
			}
		}
	}

	// The same call the dashboard's `s` makes. Locally the daemon's cached "awake" goes stale
	// for a moment, and is corrected the way it always is: the next connection dials a stopped
	// container, the belief is revoked, and it is woken again.
	if err := p.Stop(ctx, u.Ref); err != nil {
		return fail(err)
	}

	journalEvent(sandbox, u.Service, "slept", 0, nil, "slept by `sbx sleep`")

	return nil
}

// sleepLayers orders the units that hold memory (running or frozen) for stopping: each layer
// can stop in parallel, and every service is in a layer before anything it depends_on. It is
// the reverse of the order a wake walks, for the reverse reason - an app is never left running
// against a database that has already gone.
//
// Depths are taken over every unit, asleep ones included, so a dependent that is already asleep
// still orders the rest the same way. A cycle (the spec refuses one; a hand-labelled container
// could carry one) ends in the last layer rather than being dropped: stopped out of order beats
// left running.
func sleepLayers(units []provider.Unit) [][]provider.Unit {
	dependents := map[string][]string{} // service -> services that depend on it
	for _, u := range units {
		for _, d := range u.DependsOn {
			dependents[d] = append(dependents[d], u.Service)
		}
	}

	depth := map[string]int{}
	visiting := map[string]bool{}

	var depthOf func(s string) int
	depthOf = func(s string) int {
		if d, ok := depth[s]; ok {
			return d
		}

		if visiting[s] {
			return len(units) // a cycle: last
		}

		visiting[s] = true

		d := 0
		for _, dep := range dependents[s] {
			d = max(d, depthOf(dep)+1)
		}

		visiting[s] = false
		depth[s] = d

		return d
	}

	byDepth := map[int][]provider.Unit{}

	for _, u := range units {
		if u.Running || u.Paused {
			d := depthOf(u.Service)
			byDepth[d] = append(byDepth[d], u)
		}
	}

	var layers [][]provider.Unit

	for _, d := range slices.Sorted(maps.Keys(byDepth)) {
		layer := byDepth[d]
		sort.SliceStable(layer, func(i, j int) bool { return layer[i].Service < layer[j].Service })
		layers = append(layers, layer)
	}

	return layers
}

// hostVar is the companion variable for a declared port export.
//
// `DATABASE_PORT` gets `DATABASE_HOST`, which is the convention most application config
// already reads. `PGPORT` gets `PGHOST` - no underscore - because that is what libpq itself
// reads, and it is the difference between the README's `psql` example working and not: with
// PGHOST and PGPORT set, `psql` with no arguments connects to the sandbox. The same shape
// covers MYSQL_HOST/MYSQL_PORT and REDIS_HOST/REDIS_PORT without special-casing any of them.
//
// A bare `PORT` has none, as SPEC.md says: there is no name to derive, and the fallthrough's
// PORT_HOST is a variable nothing reads, set in every command `sbx env` wraps. "HOST" is not
// used either: dev servers commonly read it as the address to bind, not a peer to dial.
func hostVar(portVar string) (string, bool) {
	if portVar == "PORT" {
		return "", false
	}

	if base := strings.TrimSuffix(portVar, "_PORT"); base != portVar && base != "" {
		return base + "_HOST", true
	}

	// PGPORT → PGHOST.
	if base := strings.TrimSuffix(portVar, "PORT"); base != portVar && base != "" {
		return base + "HOST", true
	}

	return portVar + "_HOST", true
}

func isLocal(u provider.Unit) bool {
	return len(u.Client) > 0 && u.Client[0].Host == "127.0.0.1"
}

// ── exec / logs / cp ─────────────────────────────────────────────────────────
//
// The hard part of a sandbox - waking on demand, surviving sleep, being addressable - was
// already here. These three are the thin part, and without them a sandbox is a data plane
// rather than somewhere you can work.
//
// Each of them wakes what it touches, because doing anything to a sandbox is using it.

// sleepingRef finds a service's container without waking it.
//
// The counterpart to serviceRef, and the distinction is the whole point: exec, cp and the
// rest are using the sandbox, so they wake it. Reading its logs is not, so this does not.
func sleepingRef(_ context.Context, units []provider.Unit, sandbox, service string) (string, error) {
	for _, u := range units {
		if u.Service == service {
			return u.Ref, nil
		}
	}

	have := make([]string, 0, len(units))
	for _, u := range units {
		have = append(have, u.Service)
	}

	return "", fmt.Errorf("sandbox %q has no service %q (it has: %s)",
		sandbox, service, strings.Join(have, ", "))
}

// journalEvent records something the CLI did to a sandbox as an event, the shape the daemon
// writes for its own wakes and sleeps, so `sbx history` shows every one whoever caused it.
//
// history.Append directly, not logs.Default.ActorEvent as the dashboard does: that also prints
// a log line on stdout, and on `sbx exec` stdout is the command's output - a JSON line in the
// middle of `sbx exec b pg pg_dump > dump.sql` would corrupt the dump.
func journalEvent(sandbox, service, event string, took time.Duration, err error, msg string) {
	r := history.Record{
		Kind: "event", Sandbox: sandbox, Service: service, Event: event,
		DurationMs: took.Milliseconds(), Actor: logs.ActorCLI, Message: msg,
	}

	if err != nil {
		r.Failed, r.Error = true, err.Error()
	}

	history.Append(r)
}

// serviceRef finds a service and wakes it if it is asleep. by names the command for the
// journal: a wake caused by `sbx exec` is a person's, and is recorded as theirs.
func serviceRef(ctx context.Context, p provider.Provider, sandbox, service, by string) (string, error) {
	units, err := p.List(ctx, sandbox)
	if err != nil {
		return "", err
	}

	if len(units) == 0 {
		return "", UnknownSandbox(ctx, p, sandbox)
	}

	for _, u := range units {
		if u.Service == service {
			// Wake it first: exec against a stopped container fails with a message about
			// the container, not about the sandbox being asleep, which reads like a bug.
			if !u.Running {
				began := time.Now()

				err := p.Start(ctx, u.Ref)
				if err == nil {
					err = waitHealthy(ctx, p, u.Ref, "", 90*time.Second)
				}

				if err != nil {
					journalEvent(sandbox, service, "wakeFailed", time.Since(began), err, "could not wake for `sbx "+by+"`")

					return "", err
				}

				journalEvent(sandbox, service, "woke", time.Since(began), nil, "woken by `sbx "+by+"`")
			}

			return u.Ref, nil
		}
	}

	names := make([]string, 0, len(units))
	for _, u := range units {
		names = append(names, u.Service)
	}

	return "", fmt.Errorf("sandbox %q has no service %q (it has: %s)",
		sandbox, service, strings.Join(names, ", "))
}

// Exec runs a command inside a service with this process's stdio attached, and returns its
// exit status as a *ChildExit so `sbx exec` exits with it: `sbx exec b app ./check` in CI gates
// on ./check, not on sbx. With tty it hands the terminal over instead, which is what makes
// `sbx exec -t my-branch postgres psql` a usable shell.
//
// Stdin is passed on when it is a pipe or a file, not when it is a terminal: without -t there
// is no echo or line editing, so a terminal feeding a command reads as a hang, and -t is how
// to type into one. /dev/null is a character device too, and giving none is the same thing.
func Exec(ctx context.Context, p provider.Provider, sandbox, service string, argv []string, tty bool) error {
	ref, err := serviceRef(ctx, p, sandbox, service, "exec")
	if err != nil {
		return err
	}

	if tty {
		return p.ExecTTY(ctx, ref, argv)
	}

	var stdin io.Reader
	if !isCharDevice(os.Stdin) {
		stdin = os.Stdin
	}

	code, err := p.ExecStream(ctx, ref, argv, stdin, os.Stdout, os.Stderr)
	if err != nil {
		return err
	}

	if code != 0 {
		return &ChildExit{Code: code}
	}

	return nil
}

func isCharDevice(f *os.File) bool {
	info, err := f.Stat()

	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// cmdLogs shows one service, or the whole sandbox at once.
//
// With no service named it interleaves every service on stdout, each line prefixed with
// where it came from - a sandbox is a set of processes, and watching it should feel like
// watching one server rather than opening N terminals.
func Logs(ctx context.Context, p provider.Provider, sandbox, service string, lines int, follow bool) error {
	units, err := p.List(ctx, sandbox)
	if err != nil {
		return err
	}

	if len(units) == 0 {
		return UnknownSandbox(ctx, p, sandbox)
	}

	if service != "" {
		// Deliberately NOT serviceRef: that wakes what it touches, which is right for exec
		// and cp and wrong here. Asking what a sandbox said is not using it, and a `sbx logs
		// -f my-branch postgres` left open would otherwise hold a sandbox awake for as long
		// as somebody was watching it - the one command where that is exactly backwards.
		ref, err := sleepingRef(ctx, units, sandbox, service)
		if err != nil {
			return err
		}

		logs.Default.Align(len(sandbox) + 1 + len(service))

		w := &logs.LineWriter{
			Log: logs.Default, Sandbox: sandbox, Service: service, Level: logs.LevelInfo,
		}
		defer w.Flush()

		if err := p.Logs(ctx, ref, lines, follow, w); err != nil {
			return err
		}

		w.Flush()

		for _, u := range units {
			if u.Ref == ref {
				followEnded(ctx, p, sandbox, u, follow)
			}
		}

		return nil
	}

	width := 0
	for _, u := range units {
		if w := len(sandbox) + 1 + len(u.Service); w > width {
			width = w
		}
	}

	logs.Default.Align(width)

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)

	for _, u := range units {
		wg.Add(1)

		go func(u provider.Unit) {
			defer wg.Done()

			// Sleeping services are read, not woken. Asking for logs is not using the
			// sandbox, and waking three databases because somebody typed `logs` would be
			// the opposite of the point.
			w := &logs.LineWriter{
				Log:     logs.Default,
				Sandbox: sandbox,
				Service: u.Service,
				Level:   logs.LevelInfo,
			}
			defer w.Flush()

			if err := p.Logs(ctx, u.Ref, lines, follow, w); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("%s: %w", u.Service, err))
				mu.Unlock()

				return
			}

			w.Flush()
			followEnded(ctx, p, sandbox, u, follow)
		}(u)
	}

	wg.Wait()

	return errors.Join(errs...)
}

func Copy(ctx context.Context, p provider.Provider, sandbox, service, src, dst string) error {
	if strings.HasPrefix(src, ":") == strings.HasPrefix(dst, ":") {
		return fmt.Errorf("exactly one of src and dst must be inside the sandbox, written as \":path\"")
	}

	ref, err := serviceRef(ctx, p, sandbox, service, "cp")
	if err != nil {
		return err
	}

	return p.Copy(ctx, ref, src, dst)
}

// WakePort is the port a tunnel should point at for one service.
//
// Deliberately the wake port and not the workload's: a link that only works while the
// sandbox happens to be awake is a link that mostly does not work.
func WakePort(ctx context.Context, p provider.Provider, sandbox, service string) (int, error) {
	units, err := p.List(ctx, sandbox)
	if err != nil {
		return 0, err
	}

	// A mistyped sandbox is the likelier mistake, and "no service in sandbox" blamed the service.
	if len(units) == 0 {
		return 0, UnknownSandbox(ctx, p, sandbox)
	}

	for _, u := range units {
		if u.Service != service {
			continue
		}

		if len(u.Client) == 0 {
			return 0, fmt.Errorf("service %q exposes no ports", service)
		}

		if !isLocal(u) {
			return 0, fmt.Errorf(
				"in a cluster the link is an Ingress in front of %s, not a tunnel from here - "+
					"see deploy/activator.yaml", u.Client[0].Host)
		}

		return u.Client[0].Port, nil
	}

	return 0, fmt.Errorf("no service %q in sandbox %q", service, sandbox)
}

// ── list ─────────────────────────────────────────────────────────────────────

func List(ctx context.Context, p provider.Provider, asJSON bool) error {
	units, err := p.List(ctx, "")
	if err != nil {
		return err
	}

	if asJSON {
		return listJSON(os.Stdout, units, p.Name())
	}

	if len(units) == 0 {
		fmt.Printf("no sandboxes (%s)\n", p.Name())
		return nil
	}

	sort.Slice(units, func(i, j int) bool {
		if units[i].Sandbox != units[j].Sandbox {
			return units[i].Sandbox < units[j].Sandbox
		}

		return units[i].Service < units[j].Service
	})

	listTable(os.Stdout, units, p.Name())

	// The ADDRESS column is a promise only the daemon can keep.
	//
	// Those are the daemon's ports, not docker's - docker publishes a backing port and `sbx
	// serve` owns the public one - so with no daemon every address in the table refuses, while
	// the table still says awake and prints them. A running container and a reachable service
	// are different facts, and the listing showed only the first.
	//
	// Reported as a live case: `sbx ui` showed mlflow AWAKE on 127.0.0.1:20020, using 567 MB,
	// and the browser said "refused to connect". Both were true. Nothing said why.
	if !isLocal(units[0]) {
		return nil
	}

	// stderr, because the table is the answer and this is a note about it.
	//
	// Not a style point: stdout is piped, and `sbx list | grep -q name` is how everything from
	// a shell script to this repo's own use-case suite asks whether a sandbox exists. Asking
	// whether a daemon is running reads a file and signals a pid, which is long enough for grep
	// to have matched and closed the pipe - and the write that followed then took SIGPIPE and
	// killed the process, so the pipeline exited 141 under `set -o pipefail`. The table had
	// already been printed in full and correctly; only the exit status was wrong.
	if gone := unserved(units); len(gone) > 0 {
		fmt.Fprintf(os.Stderr, "\nno `sbx serve` is running for %s, so nothing accepts on "+
			"those addresses above -\na container can be awake and still unreachable. "+
			"Start one:  sbx serve --idle 5m &\n", strings.Join(gone, ", "))
	}

	return nil
}

// listJSON is the same table for something that parses rather than reads.
//
// An agent driving sbx asks what exists before it does anything, and the human table is the one
// surface that had no machine-readable form - so the answer was a regex over column widths,
// which is a thing that works until a sandbox is named something long. Flat rather than nested
// by sandbox: it mirrors the table exactly, and `jq 'map(select(.awake))'` is the question
// people actually ask of it.
//
// An empty fleet is an empty array, not an error and not silence: "there are none" and "I could
// not look" have to stay different answers, and the second one is a non-zero exit.
func listJSON(w io.Writer, units []provider.Unit, backend string) error {
	type entry struct {
		Sandbox   string   `json:"sandbox"`
		Service   string   `json:"service"`
		Awake     bool     `json:"awake"`
		State     string   `json:"state"`
		Isolation string   `json:"isolation"`
		Addresses []string `json:"addresses"`
		Ref       string   `json:"ref"`
		Provider  string   `json:"provider"`
	}

	sort.Slice(units, func(i, j int) bool {
		if units[i].Sandbox != units[j].Sandbox {
			return units[i].Sandbox < units[j].Sandbox
		}

		return units[i].Service < units[j].Service
	})

	out := make([]entry, 0, len(units))

	for _, u := range units {
		addrs := make([]string, 0, len(u.Client))
		for _, e := range u.Client {
			addrs = append(addrs, e.String())
		}

		out = append(out, entry{
			Sandbox: u.Sandbox, Service: u.Service, Awake: u.Running, State: unitState(u), Isolation: unitIsolation(u, backend),
			Addresses: addrs, Ref: u.Ref, Provider: backend,
		})
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	return enc.Encode(out)
}

// ── rm ───────────────────────────────────────────────────────────────────────

func Remove(ctx context.Context, p provider.Provider, sandbox string) error {
	if err := p.Remove(ctx, sandbox); err != nil {
		return err
	}

	// A live egress policy belongs to this sandbox, not to its name: a new sandbox created
	// under the same name starts from its own spec, not from this one's exceptions.
	if err := daemon.NewEgressControl(p, "").Forget(sandbox); err != nil {
		fmt.Printf("  (could not remove the saved egress policy: %v)\n", err)
	}

	fmt.Printf("sandbox %q destroyed\n", sandbox)

	return nil
}

// wantsAllowList reports whether any service this create will actually build declares an
// egress allow-list.
//
// Optional services are excluded unless they are being created, so a spec whose optional
// service carries the allow-list is not refused on a run that never builds it.
func wantsAllowList(sp *spec.Spec, withOptional bool) bool {
	for _, svc := range sp.Services {
		if svc.Optional && !withOptional {
			continue
		}

		if svc.Filtered() {
			return true
		}
	}

	return false
}

// unserved lists, once each and in order, the local sandboxes no running daemon fronts - neither
// the machine's nor one started with --only whose scope names them.
func unserved(units []provider.Unit) []string {
	if _, running := daemon.Running(); running {
		return nil
	}

	seen := map[string]bool{}

	var out []string

	for _, u := range units {
		if !isLocal(u) || seen[u.Sandbox] {
			continue
		}

		seen[u.Sandbox] = true

		if _, ok := daemon.Serving(u.Sandbox); !ok {
			out = append(out, u.Sandbox)
		}
	}

	return out
}

// sharedAllowList is the allow-list the sandbox's one egress filter enforces: the union of every
// created service's egress_allow, sorted, which is what separate lists have always meant
// (spec.checkEgressFilters). Each allow-list service is handed it whole.
//
// The filter container is ensured by each service's create with that service's declaration, so
// with lists of their own the second service replaced the filter with its list and the first lost
// its hosts - and since a re-run create now re-checks the filter, it would do so on every run.
func sharedAllowList(sp *spec.Spec, withOptional bool) []string {
	seen := map[string]bool{}

	var out []string

	for _, svc := range sp.Services {
		if svc.Optional && !withOptional {
			continue
		}

		for _, a := range svc.EgressAllow {
			if a = strings.TrimSpace(a); a != "" && !seen[a] {
				seen[a] = true
				out = append(out, a)
			}
		}
	}

	sort.Strings(out)

	return out
}

// followEnded says why `sbx logs -f` stopped following a service, when the reason is sleep.
//
// `docker logs --follow` ends when the container stops, and on a sandbox that sleeps that is
// routine: the idle timer fires and the command exits 0 with no word, which reads as the log
// ending or sbx failing. Following on across the next wake was the alternative, and it was not
// taken: the reattach can only start at the tail, so the first lines after a wake - the ones a
// startup failure is in - would be dropped silently, which is worse than stopping and saying so.
// u is the service as it was when the command started.
func followEnded(ctx context.Context, p provider.Provider, sandbox string, u provider.Unit, follow bool) {
	if !follow || ctx.Err() != nil {
		return // not following, or interrupted: it ended because it was asked to
	}

	now, err := p.List(ctx, sandbox)
	if err != nil {
		return
	}

	again := fmt.Sprintf("It wakes on the next connection; run `sbx logs -f %s %s` again then.", sandbox, u.Service)

	for _, n := range now {
		if n.Service != u.Service || n.Running {
			continue
		}

		if u.Running {
			fmt.Fprintf(stderr, "sbx: %s went to sleep, so there is nothing more to follow. %s\n", u.Service, again)
		} else {
			fmt.Fprintf(stderr, "sbx: %s is asleep, so there is nothing to follow - the lines above are "+
				"from before it slept. %s\n", u.Service, again)
		}
	}
}
