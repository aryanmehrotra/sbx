package provider

// The Docker provider: sandboxes as containers on one machine.
//
// Addressing here is the awkward case. Every sandbox shares one loopback, so each gets a
// block of 20 ports and every service is remapped into it - twice, because the daemon has
// to hold a port that answers while the container behind it is stopped:
//
//	client ──▶ 20002 (wake, sbx serve listens)  ──▶ 30002 (backing, docker publishes)
//
// None of that survives leaving the machine, and none of it needs to: see the Kubernetes
// provider, where a pod has its own address and MySQL is just :3306.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/aryanmehrotra/sbx/internal/spec"
	"time"
)

const (
	publicBase  = 20000
	backingBase = 30000
	blockSize   = 20
	maxSlots    = 60
)

type dockerProvider struct {
	api      *dockerClient // hot path: start, stop, health
	endpoint dockerEndpoint
}

func newDocker(ep dockerEndpoint) *dockerProvider {
	return &dockerProvider{api: newDockerClient(ep), endpoint: ep}
}

func (d *dockerProvider) Name() string { return "docker" }

// Creation shells out to `docker` while the daemon's hot path uses the Engine API. That
// split is deliberate: creation happens once and wants to be readable - you can copy the
// command out of an error and run it - whereas start, stop and inspect happen on every
// wake and want no fork.
// Both halves are pinned to the same resolved endpoint through DOCKER_HOST. Letting the CLI
// do its own resolution would mean `create` could reach one daemon and the wake another, on
// any machine with more than one context - which is most of them.
func (d *dockerProvider) docker(args ...string) (string, error) {
	cmd := exec.Command("docker", args...)
	cmd.Env = append(os.Environ(), "DOCKER_HOST="+d.endpoint.String())

	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}

	return strings.TrimSpace(string(out)), nil
}

// probeInterval is what this service asked for, or the default where nothing did.
//
// The caller resolves the sandbox's own default into the service before it gets here, so this
// is the last fallback rather than a second opinion.
func probeInterval(svc spec.Service) time.Duration {
	if d, err := time.ParseDuration(svc.HealthInterval); err == nil && d > 0 {
		return d
	}

	return spec.DefaultHealthInterval
}

func containerName(sandbox, service string) string { return "sbx-" + sandbox + "-" + service }
func volumeName(sandbox, service string) string    { return "sbx-" + sandbox + "-" + service + "-data" }

func (d *dockerProvider) Endpoints(_, _ string, slot, startIndex int, containerPorts []int) []Endpoint {
	eps := make([]Endpoint, 0, len(containerPorts))

	for i := range containerPorts {
		eps = append(eps, Endpoint{Host: "127.0.0.1", Port: publicBase + slot*blockSize + startIndex + i})
	}

	return eps
}

func (d *dockerProvider) AllocSlot(ctx context.Context, sandbox string) (int, error) {
	if slot, ok := d.slotOf(sandbox); ok {
		return slot, nil
	}

	used := map[int]bool{}

	units, err := d.List(ctx, "")
	if err != nil {
		return 0, err
	}

	for _, u := range units {
		used[u.Slot] = true
	}

	for i := range maxSlots {
		if used[i] {
			continue
		}

		// Listed as free, and then actually checked. The container list says which slots are
		// SPOKEN FOR; the ports say which are TAKEN, and between two concurrent creates those
		// are different questions - the loser of a race is handed a gap that another create
		// is about to fill, and finds out at `docker run` with "failed to set up container
		// networking". Measured before this: four concurrent creates, three failures.
		//
		// Binding is the same question docker asks a moment later, so asking it here turns a
		// raw runtime error into picking the next slot. It is a probe and not a reservation,
		// so it narrows the race rather than closing it; the lock in cli/slotlock.go covers
		// the rest for one machine, and nothing covers two machines driving one remote
		// daemon, which is why this is best-effort by design.
		if !d.backingPortsFree(i) {
			continue
		}

		return i, nil
	}

	return 0, fmt.Errorf("all %d sandbox slots are in use; destroy one first", maxSlots)
}

// Host is what docker says the machine has. See provider.Host: on macOS and Windows this is the
// VM, which is the thing the sandboxes are actually sharing.
func (d *dockerProvider) Host(ctx context.Context) (Host, error) {
	i, err := d.api.info(ctx)
	if err != nil {
		return Host{}, err
	}

	return Host{Cores: i.NCPU, MemBytes: i.MemTotal, Name: i.Name}, nil
}

// runtimeHint names the container runtime behind an endpoint, and how it is started.
//
// From the socket path, which is the only evidence there is once the thing is down: a stopped
// runtime cannot be asked what it is. Each of these puts its socket somewhere characteristic,
// and being wrong here costs a wrong command in a message rather than a wrong action.
func runtimeHint(ep dockerEndpoint) (name, start string) {
	switch {
	case strings.Contains(ep.Address, "/.colima/"):
		return "colima", "colima start"
	case strings.Contains(ep.Address, "/.rd/"):
		return "Rancher Desktop", "open -a 'Rancher Desktop'"
	case strings.Contains(ep.Address, "podman"):
		return "the podman machine", "podman machine start"
	case strings.Contains(ep.Address, "/.docker/"), strings.Contains(ep.Address, "/.lima/"):
		return "Docker Desktop", "open -a Docker"
	}

	return "the container runtime", "whatever starts it on this machine"
}

// RuntimeHint names the container runtime this machine looks like it uses, and how to start it.
//
// Exported for `sbx doctor`, which is asked exactly when nothing is answering and so cannot ask
// the runtime what it is.
func RuntimeHint() (name, start string) {
	ep, err := resolveDockerHost("")
	if err != nil {
		return "the container runtime", "whatever starts it on this machine"
	}

	return runtimeHint(ep)
}

// Neighbours is every container on this machine with what it is holding.
func (d *dockerProvider) Neighbours(ctx context.Context) ([]Neighbour, error) {
	cs, err := d.api.list(ctx, "")
	if err != nil {
		return nil, listError(d.endpoint, err)
	}

	out := make([]Neighbour, 0, len(cs))
	at := make(map[string]int, len(cs))

	var running []string

	for _, c := range cs {
		n := Neighbour{
			Name:    c.name(),
			Ours:    c.Labels[labelSandbox] != "",
			Running: c.State == "running",
		}

		if n.Running {
			running = append(running, c.ID)
			at[c.ID] = len(out)
		}

		out = append(out, n)
	}

	// A stopped container holds nothing, which is the whole point of this project, so only what
	// is running is sampled.
	usage, _ := d.Stats(ctx, running)

	for id, u := range usage {
		if i, ok := at[id]; ok {
			out[i].MemBytes = u.MemBytes
		}
	}

	return out, nil
}

// listError says what went wrong with a listing in the reader's terms.
//
// Distinguished because the two have different fixes: a socket that is not there needs the
// runtime started, and one that is not answering needs it looked at.
func listError(ep dockerEndpoint, err error) error {
	// A socket that is not there is a runtime that is not running, and that is worth saying in
	// those words. The dial error underneath - "connect: no such file or directory" against a
	// path nobody typed - describes the symptom and names neither the thing that is down nor
	// the command that brings it back.
	if ep.Network == "unix" {
		if _, statErr := os.Stat(ep.Address); os.IsNotExist(statErr) {
			name, start := runtimeHint(ep)

			return fmt.Errorf("%s is not running - %s is not there\n"+
				"     start it with `%s`. Your sandboxes survive it: they are containers, and "+
				"the first connection wakes them again",
				name, ep.Address, start)
		}
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("docker did not answer in time via %s - it is running but too busy "+
			"to reply, or wedged\n"+
			"     check with `docker ps` (it will be just as slow) or your VM's own status, "+
			"such as `colima status`", ep)
	}

	return fmt.Errorf("listing sandboxes via %s: %w", ep, err)
}

func (d *dockerProvider) slotOf(sandbox string) (int, bool) {
	out, err := d.docker("ps", "-aq", "--filter", "label="+labelSandbox+"="+sandbox)
	if err != nil || strings.TrimSpace(out) == "" {
		return 0, false
	}

	s, err := d.docker("inspect", strings.Fields(out)[0], "--format", label(labelSlot))
	if err != nil {
		return 0, false
	}

	n, err := strconv.Atoi(s)

	return n, err == nil
}

func label(name string) string { return "{{index .Config.Labels \"" + name + "\"}}" }

func (d *dockerProvider) Create(_ context.Context, sandbox string, slot, _ int, service string,
	svc spec.Service, eps []Endpoint, specDir string, iso Isolation,
) error {
	cn := containerName(sandbox, service)

	// Before the container joins it, and only when something asks. Fails closed: a service
	// that declared "deny" and could not get the network must not start with egress open.
	if svc.Egress == spec.EgressDeny || len(svc.EgressAllow) > 0 {
		if err := d.ensureEgressNetwork(sandbox); err != nil {
			return err
		}
	}

	if _, err := d.docker("inspect", cn); err == nil {
		fmt.Printf("  %-12s already exists\n", service)
		return nil
	}

	var wake, backing []string

	for _, e := range eps {
		wake = append(wake, strconv.Itoa(e.Port))
		backing = append(backing, strconv.Itoa(e.Port-publicBase+backingBase))
	}

	args := []string{"run", "-d", "--name", cn,
		"--label", labelSandbox + "=" + sandbox,
		"--label", labelSlot + "=" + strconv.Itoa(slot),
		"--label", labelService + "=" + service,
		"--label", labelPorts + "=" + pairLabel(wake, backing),
	}

	// A non-default runtime is how the isolation tier is actually applied. Locally this is
	// empty and the container shares the host kernel, which is the right trade for one
	// user; on shared infrastructure runsc or kata-runtime goes here instead.
	if rt := dockerRuntime(iso); rt != "" {
		args = append(args, "--runtime", rt)
	}

	// Passed verbatim: "all", "1", "device=0". Docker refuses an unknown value itself,
	// and its error names the runtime that is missing - better than anything we would
	// paraphrase.
	if svc.CPU != "" {
		args = append(args, "--cpus", svc.CPU)
	}

	if svc.Memory != "" {
		args = append(args, "--memory", svc.Memory)
	}

	for _, c := range svc.CapAdd {
		if c = strings.TrimSpace(c); c != "" {
			args = append(args, "--cap-add", c)
		}
	}

	if svc.GPUs != "" {
		args = append(args, "--gpus", svc.GPUs)
	}

	if len(svc.DependsOn) > 0 {
		args = append(args, "--label", labelDependsOn+"="+strings.Join(svc.DependsOn, ","))
	}

	if svc.Idle != "" {
		args = append(args, "--label", labelIdle+"="+svc.Idle)
	}

	// An allow-list gets the no-NAT bridge too - direct egress denied - plus a filtering proxy
	// on the gateway as its one way out. HTTP(S)_PROXY points ordinary clients at it; a client
	// that ignores the proxy and dials out directly has no route, so the allow-list holds.
	if len(svc.EgressAllow) > 0 {
		gw, err := d.egressGateway(sandbox)
		if err != nil {
			return err
		}

		// The filter is a listener the daemon opens ON that gateway, and the daemon runs
		// wherever you are - which on a VM-backed docker is not where the bridge is. Colima
		// and Docker Desktop put the bridge inside the Linux VM, so 172.x.0.1 exists there
		// and not on this machine, and the bind fails with "can't assign requested address".
		//
		// Left to the daemon this is a warning every refresh tick and a sandbox that was
		// reported created: the service comes up, reports healthy, and has no egress at all -
		// not to the allowed hosts either. Fails closed, which is the safe direction and the
		// wrong report. `--isolation gvisor|kata` and `egress: "deny"` on kubernetes are both
		// refused up front for the same reason, and this is the same shape.
		// Where the daemon can hold that address it does, and the filter is a listener in the
		// daemon - fewer moving parts, and it already knows which units are awake. Where it
		// cannot, the same filter runs as a container on the bridge instead, which is on the
		// right side of the VM boundary by construction. Either way the workload has no route
		// out of its own, so the proxy is the only door.
		proxyHost, stat := gw, ""

		if err := bindable(gw); err != nil {
			addr, cerr := d.ensureFilterContainer(sandbox, svc.EgressAllow)
			if cerr != nil {
				return fmt.Errorf("%w\n\nthe filter could not be run as a container either: %v", err, cerr)
			}

			proxyHost, stat = filterAlias, addr
		}

		proxy := "http://" + net.JoinHostPort(proxyHost, strconv.Itoa(EgressProxyPort))
		for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
			args = append(args, "-e", k+"="+proxy)
		}

		args = append(args,
			"--label", labelEgressAllow+"="+strings.Join(svc.EgressAllow, ","),
			"--label", labelEgressGateway+"="+gw)

		if stat != "" {
			args = append(args, "--label", labelEgressStat+"="+stat)
		}
	}

	if svc.Egress == spec.EgressDeny || len(svc.EgressAllow) > 0 {
		args = append(args, "--network", egressNetwork(sandbox))

		// The service's own name, on the network, as well as the container's.
		//
		// Everything a spec says addresses a service by its short name - `depends_on: ["db"]`,
		// `sbx logs x db`, the key in `services` - but docker's embedded DNS only knows the
		// container, which is `sbx-<sandbox>-<service>`. So a spec that declares `depends_on`
		// and then configures the dependent with `db:6379` gets `no such host`: the dependency
		// is woken, correctly, and still cannot be reached. Measured: `db` answers NXDOMAIN on
		// the sandbox network while `sbx-deplab-db` resolves.
		//
		// That error is indistinguishable from the one dependency-ordered wake exists to
		// prevent, which is what makes it expensive to diagnose. An alias costs nothing and
		// makes the spec's own vocabulary work on the wire.
		args = append(args, "--network-alias", service)
	}

	for i := range eps {
		args = append(args, "-p", fmt.Sprintf("127.0.0.1:%s:%d", backing[i], svc.Ports[i]))
	}

	if svc.Health != "" {
		// The interval is the floor on how long a wake appears to take, because docker only
		// re-evaluates health on it. The long start period is the opposite of low retries:
		// inside it a failing check does not latch the container as unhealthy, while a
		// passing one still flips it immediately - so a database that needs six seconds to
		// open its data directory is not declared broken at 300ms.
		args = append(args,
			"--health-cmd", svc.Health,
			"--health-interval", probeInterval(svc).String(),
			"--health-timeout", "2s",
			"--health-retries", "3",
			"--health-start-period", "60s",
		)
	}

	for _, k := range SortedKeys(svc.Env) {
		args = append(args, "-e", k+"="+svc.Env[k])
	}

	if svc.Volume != "" {
		args = append(args, "-v", volumeName(sandbox, service)+":"+svc.Volume)
	}

	for _, hostPath := range SortedKeys(svc.Files) {
		abs := hostPath
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(specDir, hostPath)
		}

		// Absolute, always. Docker reads a RELATIVE `-v` source as a named volume, not a
		// path - so it silently creates an empty volume and mounts that, and the container
		// finds a directory where its config should be. `sbx create --spec sandbox.json`
		// (relative, which is the normal way to type it) made specDir "." and left the
		// source relative, so `files:` did not work at all from the working directory.
		//
		// The mount check downstream caught the symptom and blamed VM path sharing, which is
		// a real cause of the same symptom and was the wrong one here.
		resolved, err := filepath.Abs(abs)
		if err != nil {
			return fmt.Errorf("service %q: file %s: %w", service, hostPath, err)
		}

		abs = resolved

		if _, err := os.Stat(abs); err != nil {
			return fmt.Errorf("service %q: file %s: %w", service, hostPath, err)
		}

		args = append(args, "-v", abs+":"+svc.Files[hostPath]+":ro")
	}

	// Read-write, and the host side is created if it is missing.
	//
	// Docker creates a missing bind source itself, as root - so the first run of a spec that
	// mounts ./data leaves a directory the user cannot write to, and the failure surfaces
	// later as a permission error from inside the container. Creating it here means it belongs
	// to whoever ran sbx, which is the only owner that makes sense for a directory they are
	// going to open.
	for _, hostPath := range SortedKeys(svc.Mounts) {
		abs := hostPath
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(specDir, hostPath)
		}

		abs, err := filepath.Abs(abs)
		if err != nil {
			return fmt.Errorf("mount %q: %w", hostPath, err)
		}

		if _, err := os.Stat(abs); os.IsNotExist(err) {
			if err := os.MkdirAll(abs, 0o755); err != nil {
				return fmt.Errorf("mount %q: could not create %s: %w", hostPath, abs, err)
			}
		}

		args = append(args, "-v", abs+":"+svc.Mounts[hostPath])
	}

	args = append(args, svc.Image)
	args = append(args, svc.Args...)

	_, err := d.docker(args...)

	return err
}

func egressNetwork(sandbox string) string { return "sbx-noegress-" + sandbox }

// egressGateway returns the gateway IP of a sandbox's no-NAT bridge - the host address a
// container on it can reach (local traffic, so it needs no route out), and where the egress
// filter binds. Read after the network exists, so an allow-list can point HTTP_PROXY at it.
func (d *dockerProvider) egressGateway(sandbox string) (string, error) {
	gw, err := d.docker("network", "inspect", egressNetwork(sandbox),
		"--format", "{{(index .IPAM.Config 0).Gateway}}")
	if err != nil {
		return "", fmt.Errorf("finding the egress gateway for %s: %w", sandbox, err)
	}

	if gw = strings.TrimSpace(gw); gw == "" {
		return "", fmt.Errorf("the egress bridge for %s has no gateway address", sandbox)
	}

	return gw, nil
}

// ensureEgressNetwork creates a bridge with IP masquerade disabled.
//
// Without masquerade there is no NAT off the host, so nothing routed leaves - and docker
// still publishes ports into it, so the wake path is untouched. That last part is why this
// works and the obvious alternatives do not: `--internal` and `--network none` both block
// egress AND stop publishing, which produces a sandbox that can never be woken.
//
// Measured 2026-08-15: published port answered 200, external fetch blocked, docker's
// embedded DNS still resolved - it sits on the bridge and needs no route out.
func (d *dockerProvider) ensureEgressNetwork(sandbox string) error {
	name := egressNetwork(sandbox)

	if _, err := d.docker("network", "inspect", name); err == nil {
		return nil
	}

	if _, err := d.docker("network", "create",
		"-o", "com.docker.network.bridge.enable_ip_masquerade=false",
		"--label", labelSandbox+"="+sandbox, name); err != nil {
		// Racing another service of the same sandbox is not a failure.
		if _, e := d.docker("network", "inspect", name); e == nil {
			return nil
		}

		return fmt.Errorf("service declared egress deny and the network could not be created: %w", err)
	}

	return nil
}

func dockerRuntime(iso Isolation) string {
	switch iso {
	case IsolationGVisor:
		return "runsc"
	case IsolationKata:
		return "kata-runtime"
	default:
		return ""
	}
}

func pairLabel(wake, backing []string) string {
	parts := make([]string, 0, len(wake))
	for i := range wake {
		parts = append(parts, wake[i]+":"+backing[i])
	}

	return strings.Join(parts, ",")
}

func (d *dockerProvider) Start(ctx context.Context, ref string) error {
	return d.api.start(ctx, ref)
}

func (d *dockerProvider) Stop(ctx context.Context, ref string) error {
	return d.api.stop(ctx, ref, 10*time.Second)
}

func (d *dockerProvider) Healthy(ctx context.Context, ref string) (bool, bool) {
	return d.api.healthy(ctx, ref)
}

// Probe runs the container's own HEALTHCHECK command inside it, right now.
//
// Docker already knows how to answer this and answers it late: it re-evaluates health on
// the check interval and republishes afterwards, which measured 5030ms on a container that
// was serving at 110ms. Running the same command ourselves costs one exec and returns in
// about 150ms, and the command is the one the spec declared - so this is faster without
// being a different question.
func (d *dockerProvider) Probe(ctx context.Context, ref string) (bool, bool) {
	cmd, ok := d.api.healthCommand(ctx, ref)
	if !ok {
		return false, false
	}

	// Over the Engine API, not `docker exec`. This is the wake path and it is asked
	// repeatedly until the workload answers, so every poll used to be a process spawn plus
	// the docker CLI's own startup.
	//
	// Honestly: this did NOT measurably improve wake latency. Interleaved A/B on the same
	// machine, n=8 each: 228 ms median via the CLI against 252 ms via the API, spreads of
	// 210-754 and 155-452 - a difference well inside the noise, in the unflattering
	// direction. The reason to keep it is the same one that took `docker ps` + N ×
	// `docker inspect` out of List: it removes a process spawn from a path the daemon walks
	// on its own schedule, which is what stops cost growing with the number of sandboxes.
	// The wake budget is dominated by `docker start`, not by how the health check is asked.
	code, err := d.api.exec(ctx, ref, []string{"sh", "-c", cmd})
	if err != nil {
		return false, true
	}

	return code == 0, true
}

func (d *dockerProvider) Commit(_ context.Context, ref, image string) error {
	// Pausing for the duration of the copy is what makes this crash-consistent rather than
	// torn - the filesystem does not move underneath it. It is docker's default and the flag
	// that used to say so is deprecated, so passing it printed a deprecation notice on top of
	// every commit error, burying the actual reason. Not passing it keeps the behaviour and
	// loses the noise; `--no-pause` is the flag that would change it.
	_, err := d.docker("commit", ref, image)

	return err
}

// checkpointReady returns nil when this daemon can CRIU-checkpoint, or the honest refusal
// when it cannot. `docker checkpoint` lives behind the daemon's experimental flag, which
// Docker Desktop (and therefore every macOS host) leaves off - so this is where the "sleeping
// keeps the disk, not the process" limitation is stated once, before anything is half done.
// isPodman reports whether the endpoint is podman rather than dockerd. It matters because the
// two implement CRIU differently, and only one of them implements it well: docker's
// checkpoint/restore is unmaintained and its RESTORE fails on the same host where podman's
// succeeds (measured - see DECISIONS.md). Detected by the socket name first, and confirmed by
// the version components when the socket is generic.
func (d *dockerProvider) isPodman() bool {
	if strings.Contains(d.endpoint.Address, "podman") {
		return true
	}

	out, err := d.docker("version", "--format", "{{range .Server.Components}}{{.Name}};{{end}}")

	return err == nil && strings.Contains(strings.ToLower(out), "podman")
}

// podman runs the podman CLI against the same endpoint. Used only for checkpoint/restore,
// where podman's native command is reliable and docker's compat API is not.
func (d *dockerProvider) podman(args ...string) (string, error) {
	cmd := exec.Command("podman", append([]string{"--url", d.endpoint.String()}, args...)...)

	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("podman %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}

	return strings.TrimSpace(string(out)), nil
}

func (d *dockerProvider) checkpointReady() error {
	// podman needs no experimental flag; it needs CRIU on the host and its own CLI here, and
	// says so clearly itself if CRIU is missing. This is the path that actually restores.
	if d.isPodman() {
		if _, err := exec.LookPath("podman"); err != nil {
			return fmt.Errorf("memory checkpoint against a podman runtime drives `podman "+
				"container checkpoint`, whose CRIU integration restores where docker's does not "+
				"- but the podman CLI is not on PATH here: %w", err)
		}

		return nil
	}

	exp, err := d.docker("info", "--format", "{{.ExperimentalBuild}}")
	if err != nil {
		return fmt.Errorf("cannot tell whether this docker daemon supports checkpoint: %w", err)
	}

	if exp != "true" {
		return fmt.Errorf("memory checkpoint needs docker's experimental checkpoint/restore " +
			"API (CRIU), and this daemon reports experimental=false - Docker Desktop and Colima " +
			"on macOS do not enable it. Filesystem snapshot (sbx snapshot / fork) works here; a " +
			"memory checkpoint needs a Linux host with a daemon started --experimental (or a " +
			"podman runtime, whose restore is the reliable one). sbx doctor reports this as " +
			"`docker checkpoint`")
	}

	return nil
}

func (d *dockerProvider) Checkpoint(_ context.Context, ref, name string, leaveRunning bool) error {
	if err := d.checkpointReady(); err != nil {
		return err
	}

	if d.isPodman() {
		// Podman checkpoints in place: the dump is held with the container, so resume needs no
		// name. `name` is sbx's label - podman keeps the most recent checkpoint per container.
		// This is the branch that actually works: a redis with no on-disk persistence, its key
		// set only in memory, comes back with the key present after resume.
		args := []string{"container", "checkpoint"}
		if leaveRunning {
			args = append(args, "--leave-running")
		}

		_, err := d.podman(append(args, ref)...)

		return err
	}

	args := []string{"checkpoint", "create"}
	if leaveRunning {
		args = append(args, "--leave-running")
	}

	_, err := d.docker(append(args, ref, name)...)

	return err
}

func (d *dockerProvider) Restore(_ context.Context, ref, name string) error {
	if err := d.checkpointReady(); err != nil {
		return err
	}

	if d.isPodman() {
		// In-place restore of the container podman froze; the name is not part of podman's
		// model, so it is accepted for API symmetry and not passed on.
		_, err := d.podman("container", "restore", ref)

		return err
	}

	// A restore is a start that seeds the container from the CRIU image instead of its
	// entrypoint. The container has to exist and be stopped, which is exactly the state a
	// non-leave-running Checkpoint left it in.
	_, err := d.docker("start", "--checkpoint", name, ref)

	return err
}

func (d *dockerProvider) Checkpoints(_ context.Context, ref string) ([]string, error) {
	if err := d.checkpointReady(); err != nil {
		return nil, err
	}

	if d.isPodman() {
		// Podman holds one in-place checkpoint per container; report whether this one has it.
		out, err := d.podman("container", "inspect", "--format", "{{.State.Checkpointed}}", ref)
		if err != nil {
			return nil, err
		}

		if strings.TrimSpace(out) == "true" {
			return []string{"checkpoint"}, nil
		}

		return nil, nil
	}

	out, err := d.docker("checkpoint", "ls", ref)
	if err != nil {
		return nil, err
	}

	var names []string

	for i, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if i == 0 || line == "" { // skip the "CHECKPOINT NAME" header
			continue
		}

		names = append(names, strings.Fields(line)[0])
	}

	return names, nil
}

func (d *dockerProvider) Images(_ context.Context, prefix string) ([]string, error) {
	out, err := d.docker("images", "--format", "{{.Repository}}:{{.Tag}}", "--filter",
		"reference="+prefix+"*")
	if err != nil {
		return nil, err
	}

	var names []string

	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			names = append(names, line)
		}
	}

	return names, nil
}

// CopyVolume copies volume to volume through a throwaway container.
//
// `cp -a` inside a small image is docker's own recipe for this and it is the right one:
// it stays in docker's storage, needs no host path (which colima would not share anyway),
// preserves ownership and permissions - postgres refuses to start on a data directory it
// does not own - and never streams the bytes through this process.
func (d *dockerProvider) CopyVolume(_ context.Context, src, dst string) error {
	// Two things this deliberately does, both learned the hard way.
	//
	// It REPLACES rather than merges. `cp -a /from/.` on its own lands the snapshot on top of
	// whatever Create just initialised, so files present only in the fresh data directory
	// survive into what is supposed to be an exact copy - a database whose state is neither
	// the snapshot's nor a fresh one's.
	//
	// And it lets the exit code mean something. This used to end in `2>/dev/null || true`,
	// which made the container exit 0 unconditionally: a full disk, an unreadable source, a
	// volume that did not exist - all reported success, on the one primitive in this project
	// that moves user data. DECISIONS.md records what that failure looks like when it
	// happens: a fork with a working server and an empty database, which looks like it
	// worked. The mechanism changed; the shape was still reachable.
	// The source is checked BEFORE the destination is touched, which the first version of
	// this got wrong: it counted both sides afterwards and compared them, so an empty source
	// gave 0 == 0 and passed - after the delete had already wiped a freshly initialised data
	// directory. Reachable in practice, because `sbx gc --snapshots --force` collects a
	// snapshot's image and its volume as two separate artifacts, so an interrupted sweep can
	// leave an image that SnapshotsOf still resolves with no volume behind it.
	//
	// Counting counts every entry, not just regular files: a copy that silently dropped every
	// symlink or empty directory would otherwise still report the same number on both sides.
	script := `set -e
if [ -z "$(find /from -mindepth 1 -print -quit)" ]; then
  echo "SBXEMPTY"
  exit 0
fi
find /to -mindepth 1 -delete
cp -a /from/. /to/
echo "SBXCOUNT $(find /from -mindepth 1 | wc -l) $(find /to -mindepth 1 | wc -l)"`

	out, err := d.docker("run", "--rm",
		"-v", src+":/from:ro",
		"-v", dst+":/to",
		"alpine:3", "sh", "-c", script)
	if err != nil {
		return fmt.Errorf("copying volume %s to %s: %w: %s", src, dst, err, lastLines(out, 8))
	}

	// Docker creates an empty volume for a name that does not exist rather than failing, so
	// "the source has nothing in it" and "the source is not there" look identical from here.
	// Both are refused, because neither is a snapshot worth restoring and the alternative is
	// a fork with a working server and an empty database.
	if strings.Contains(out, "SBXEMPTY") {
		return fmt.Errorf("copying volume %s to %s: the source is empty or does not exist - "+
			"nothing was changed", src, dst)
	}

	// The copy is asserted, not assumed.
	from, to, ok := parseCopyCount(out)
	if !ok {
		return fmt.Errorf("copying volume %s to %s: the copy did not report what it moved: %s",
			src, dst, lastLines(out, 8))
	}

	if from != to {
		return fmt.Errorf("copying volume %s to %s: %d files in the source and %d in the "+
			"destination - the copy was incomplete", src, dst, from, to)
	}

	return nil
}

// parseCopyCount reads the file counts CopyVolume's script reports.
func parseCopyCount(out string) (from, to int, ok bool) {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[0] != "SBXCOUNT" {
			continue
		}

		f, err1 := strconv.Atoi(fields[1])

		t, err2 := strconv.Atoi(fields[2])
		if err1 != nil || err2 != nil {
			return 0, 0, false
		}

		return f, t, true
	}

	return 0, 0, false
}

func (d *dockerProvider) Build(_ context.Context, tag, contextDir, dockerfile string) error {
	out, err := d.docker("build", "-t", tag, "-f", filepath.Join(contextDir, dockerfile), contextDir)
	if err != nil {
		return fmt.Errorf("building %s: %w: %s", tag, err, lastLines(out, 12))
	}

	return nil
}

func (d *dockerProvider) HasImage(_ context.Context, tag string) (bool, error) {
	if _, err := d.docker("image", "inspect", tag); err != nil {
		return false, nil // absent, not broken: inspect fails the same way for both
	}

	return true, nil
}

func (d *dockerProvider) Pull(_ context.Context, image string) error {
	out, err := d.docker("pull", "-q", image)
	if err != nil {
		return fmt.Errorf("pulling %s: %w: %s", image, err, lastLines(out, 6))
	}

	return nil
}

// lastLines keeps a build failure readable. Docker's output is long and the reason is at
// the end; printing all of it buries the line someone needs.
func lastLines(s string, n int) string {
	ls := strings.Split(strings.TrimSpace(s), "\n")
	if len(ls) > n {
		ls = ls[len(ls)-n:]
	}

	return strings.Join(ls, "\n")
}

func (d *dockerProvider) VolumeFor(sandbox, service string) string {
	return volumeName(sandbox, service)
}

func (d *dockerProvider) ExecTTY(ctx context.Context, ref string, argv []string) error {
	// -i always, -t only when stdin really is a terminal: asking docker for a TTY when
	// stdin is a pipe makes it refuse outright, which would break `sbx exec -t` inside a
	// script for no reason the user could see.
	flags := []string{"exec", "-i"}
	if isTerminal(os.Stdin) {
		flags = append(flags, "-t")
	}

	cmd := exec.CommandContext(ctx, "docker", append(append(flags, ref), argv...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr

	return cmd.Run()
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}

	return info.Mode()&os.ModeCharDevice != 0
}

func (d *dockerProvider) Exec(_ context.Context, ref string, argv []string) (string, error) {
	return d.docker(append([]string{"exec", ref}, argv...)...)
}

func (d *dockerProvider) Logs(ctx context.Context, ref string, lines int, follow bool, w io.Writer) error {
	args := []string{"logs", "--tail", strconv.Itoa(lines)}
	if follow {
		args = append(args, "--follow")
	}

	return d.stream(ctx, w, append(args, ref)...)
}

// stream runs a docker command and copies both its streams to w as they arrive. Containers
// disagree about which one logs belong on, and a viewer does not care.
func (d *dockerProvider) stream(ctx context.Context, w io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env = append(os.Environ(), "DOCKER_HOST="+d.endpoint.String())
	cmd.Stdout = w
	cmd.Stderr = w

	err := cmd.Run()
	if ctx.Err() != nil {
		return nil
	}

	return err
}

func (d *dockerProvider) Copy(_ context.Context, ref, src, dst string) error {
	_, err := d.docker("cp", qualify(ref, src), qualify(ref, dst))
	return err
}

// qualify turns ":/etc/hosts" into "<ref>:/etc/hosts" and leaves host paths alone. The
// leading colon is the whole syntax: it says "this side is inside".
func qualify(ref, path string) string {
	if strings.HasPrefix(path, ":") {
		return ref + path
	}

	return path
}

// List rebuilds every sandbox from the labels docker is already holding.
//
// One HTTP request over the socket, not one `docker ps` plus one `docker inspect` per
// container. That mattered more than it looks: List is called by discover() on every refresh
// tick, by AllocSlot on every create, and by nine CLI commands - so on a machine with twenty
// sandboxes the daemon was spawning dozens of docker CLI processes every fifteen seconds,
// for ever, and `sbx create` got slower the more sandboxes already existed. The Engine API
// returns labels, state and names in the same response the filter already needs.
//
// The API client this uses was written for exactly this and had no callers.
func (d *dockerProvider) List(ctx context.Context, sandbox string) ([]Unit, error) {
	filter := labelSandbox
	if sandbox != "" {
		filter += "=" + sandbox
	}

	cs, err := d.api.list(ctx, filter)
	if err != nil {
		// A daemon that is slow rather than absent is the case this used to report worst.
		// "context deadline exceeded", wrapped around an escaped URL, describes what the Go
		// runtime did and not what happened - and what happened is that docker was asked to
		// list containers and had not answered by the time the deadline passed. On a VM-backed
		// runtime under load that is minutes, not seconds: measured at 1m36s for seven
		// containers on a busy colima, where the same call is milliseconds on an idle one.
		//
		// Distinguished from unreachable because the two have different fixes: a socket that is
		// not there needs the runtime started, and one that is not answering needs it looked at.
		// Returning (nil, nil) here reported "no sandboxes" whenever docker was unreachable,
		// which is the same lie as a collector that cannot run and exits 0. An unreachable
		// daemon is an error, not an empty list.
		return nil, listError(d.endpoint, err)
	}

	var units []Unit

	for _, c := range cs {
		pairs, err := ParsePorts(c.Labels[labelPorts])
		if err != nil || len(pairs) == 0 {
			// A container carrying the sandbox label but no parseable ports is not something
			// sbx can front. Skipping it keeps `sbx list` working rather than failing whole.
			continue
		}

		slot, _ := strconv.Atoi(c.Labels[labelSlot])

		u := Unit{
			Sandbox:       c.Labels[labelSandbox],
			Service:       c.Labels[labelService],
			Slot:          slot,
			Ref:           c.name(),
			Instance:      c.ID,
			Running:       c.State == "running",
			Index:         (pairs[0].Public - publicBase) % blockSize,
			EgressGateway: c.Labels[labelEgressGateway],
			Idle:          c.Labels[labelIdle],
			EgressStat:    c.Labels[labelEgressStat],
		}

		if a := c.Labels[labelEgressAllow]; a != "" {
			u.EgressAllow = strings.Split(a, ",")
		}

		if dep := c.Labels[labelDependsOn]; dep != "" {
			u.DependsOn = strings.Split(dep, ",")
		}

		for _, pr := range pairs {
			u.Client = append(u.Client, Endpoint{Host: "127.0.0.1", Port: pr.Public})
			u.Listen = append(u.Listen, pr.Public)
			u.Upstream = append(u.Upstream, Endpoint{Host: "127.0.0.1", Port: pr.Backing})
		}

		units = append(units, u)
	}

	// Deterministic, because the API returns creation order and half a dozen callers render
	// this to a terminal. `sbx list` reordering itself between runs reads as churn.
	sort.Slice(units, func(i, j int) bool {
		if units[i].Sandbox != units[j].Sandbox {
			return units[i].Sandbox < units[j].Sandbox
		}

		return units[i].Service < units[j].Service
	})

	return units, nil
}

func (d *dockerProvider) Remove(ctx context.Context, sandbox string) error {
	units, err := d.List(ctx, sandbox)
	if err != nil {
		return err
	}

	if len(units) == 0 {
		return fmt.Errorf("no sandbox %q", sandbox)
	}

	for _, u := range units {
		if _, err := d.docker("rm", "-f", u.Ref); err != nil {
			return err
		}

		fmt.Printf("  removed %s\n", u.Ref)

		if _, err := d.docker("volume", "rm", volumeName(sandbox, u.Service)); err == nil {
			fmt.Printf("  removed volume %s\n", volumeName(sandbox, u.Service))
		}
	}

	// Before the network: the filter sits on it, and docker will not remove a network that
	// still has a container attached. A filter that outlived its sandbox would be a container
	// nothing owns, holding a published port, which is the failure sandboxes exist to avoid.
	d.removeFilterContainer(sandbox)

	// After the containers: docker refuses to remove a network still in use. A sandbox that
	// never declared egress deny has none, and the failure is ignored - this is cleanup, and
	// reporting "no such network" would make every ordinary rm look like it went wrong.
	if _, err := d.docker("network", "rm", egressNetwork(sandbox)); err == nil {
		fmt.Printf("  removed network %s\n", egressNetwork(sandbox))
	}

	return nil
}

// backingPortsFree reports whether a slot's docker-published ports can be bound.
//
// The backing ports, not the public ones: docker publishes on 30000+, and the public ports
// belong to the daemon, which may not be running when a sandbox is created. Probing the wrong
// half of the pair is a check that always passes.
//
// Only the first few of the block are tried. A slot is claimed by its first service, so a
// collision shows up there, and binding twenty sockets per candidate slot to be thorough
// would cost more than the race does.
func (d *dockerProvider) backingPortsFree(slot int) bool {
	for i := range 3 {
		port := backingBase + slot*blockSize + i

		ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
		if err != nil {
			return false
		}

		_ = ln.Close()
	}

	return true
}

// Stats implements Meter.
//
// Concurrent, because one round trip per container against a busy docker daemon is the
// difference between a dashboard that redraws in 40ms and one that visibly stutters at twenty
// sandboxes. Bounded, because firing two hundred requests at a laptop's docker daemon to draw
// a table is its own kind of rude.
// Limits reports what one container is allowed. A container that is asleep has no limits to
// report and says so with an error, the same way Stats omits it.
func (d *dockerProvider) Limits(ctx context.Context, ref string) (Limits, error) {
	h, err := d.api.limits(ctx, ref)
	if err != nil {
		return Limits{}, err
	}

	return Limits{
		NanoCPUs: h.HostConfig.NanoCpus,
		MemBytes: h.HostConfig.Memory,
	}, nil
}

// SetLimits changes what one container is allowed, without recreating it.
//
// It refuses to remove one, and the refusal belongs here rather than in a caller because it
// is docker's rule and not everybody's: a cluster deletes a field and is done. Docker's update
// endpoint reads a zero as "leave this alone", so asking it to clear a ceiling succeeds,
// changes nothing, and reports success - verified against a live daemon. Refusing out loud is
// the only one of the three behaviours that is not a lie.
func (d *dockerProvider) SetLimits(ctx context.Context, ref string, l Limits) error {
	cur, err := d.Limits(ctx, ref)
	if err == nil {
		switch {
		case l.NanoCPUs == 0 && cur.NanoCPUs > 0:
			return errCannotClear("cpu")
		case l.MemBytes == 0 && cur.MemBytes > 0:
			return errCannotClear("memory")
		}
	}

	if err := d.api.update(ctx, ref, l.NanoCPUs, l.MemBytes); err != nil {
		return fmt.Errorf("could not set limits on %s: %w", ref, err)
	}

	return nil
}

func errCannotClear(which string) error {
	return fmt.Errorf("docker cannot remove a %s limit from a container that exists - "+
		"recreate the sandbox to clear it", which)
}

func (d *dockerProvider) Stats(ctx context.Context, refs []string) (map[string]Usage, error) {
	const parallel = 8

	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		out = make(map[string]Usage, len(refs))
		sem = make(chan struct{}, parallel)
	)

	for _, ref := range refs {
		wg.Add(1)

		go func(ref string) {
			defer wg.Done()

			sem <- struct{}{}
			defer func() { <-sem }()

			s, err := d.api.stats(ctx, ref)
			if err != nil {
				// A container that stopped between the listing and this call is the normal
				// case, not a failure: it went to sleep, which is what it is for. Omitted,
				// and the caller reads a missing entry as zero.
				return
			}

			// Docker's own CLI subtracts the page cache. Leaving it in reports a database
			// that has read a large table as though it were holding all of it.
			mem := s.Memory.Usage
			if mem > s.Memory.Stats.InactiveFile {
				mem -= s.Memory.Stats.InactiveFile
			}

			mu.Lock()
			out[ref] = Usage{
				CPUNanos:    s.CPU.Usage.Total,
				SystemNanos: s.CPU.System,
				OnlineCPUs:  s.CPU.OnlineCPUs,
				MemBytes:    mem,
				MemLimit:    s.Memory.Limit,
			}
			mu.Unlock()
		}(ref)
	}

	wg.Wait()

	return out, nil
}

// EgressPreflight reports whether the filtering proxy an allow-list needs can be run here at all.
//
// It creates the sandbox's egress network to ask, because the gateway address does not exist
// until the network does - and that network is created for this sandbox anyway a moment later.
//
// Two ways to run the filter, and only the second can fail here. The daemon binds the bridge
// gateway where it holds that address; where it does not - a VM-backed docker, which is every
// Mac - the same filter runs as a container on the bridge, which needs nothing of this machine.
// So the refusal that used to be the whole answer now applies only where neither works.
func (d *dockerProvider) EgressPreflight(_ context.Context, sandbox string) error {
	if err := d.ensureEgressNetwork(sandbox); err != nil {
		return err
	}

	gw, err := d.egressGateway(sandbox)
	if err != nil {
		return err
	}

	// The container filter needs a docker that can run one, which is any docker at all. Nothing
	// left to refuse: the bind is no longer the only way to enforce a list.
	if err := bindable(gw); err == nil {
		return nil
	}

	if _, err := d.docker("version", "--format", "{{.Server.Version}}"); err != nil {
		// Put the network back, for the same reason the old refusal did.
		_, _ = d.docker("network", "rm", egressNetwork(sandbox))

		return fmt.Errorf(
			"declares egress_allow, and the filtering proxy that enforces it cannot be "+
				"started: this machine cannot bind the sandbox's gateway %s, and the "+
				"fallback - running the filter as a container on the bridge - needs a "+
				"docker that answers, which this one does not (%v)", gw, err)
	}

	return nil
}

// bindable reports whether this machine can open a listener on the sandbox's bridge gateway,
// which is where the egress filter has to sit.
//
// The daemon opens that listener, and the daemon runs wherever you are - which on a VM-backed
// docker is not where the bridge is. Colima and Docker Desktop keep it inside the Linux VM, as
// do rootless docker (its own netns) and a remote DOCKER_HOST (another machine), so 172.x.0.1
// exists over there and not here.
//
// Left to the daemon this was a warning every refresh tick against a sandbox that had been
// reported created: the service came up healthy and had no egress at all - not even to the
// allowed hosts. Measured from inside such a box, api.anthropic.com answered 000. It fails
// closed, which is the safe direction and the wrong report.
//
// Port 0 rather than the proxy's own port, deliberately: this asks whether the ADDRESS is one
// this machine holds, and asking about 20999 would refuse whenever the daemon legitimately
// holds it already for a sibling service on the same gateway.
func bindable(gw string) error {
	probe, err := net.Listen("tcp", net.JoinHostPort(gw, "0"))
	if err != nil {
		return fmt.Errorf(
			"declares egress_allow, and the filtering proxy that enforces it cannot be "+
				"started: the sandbox's gateway %s is not an address this machine can "+
				"bind (%v).\n"+
				"That is what a VM-backed docker looks like - colima and Docker Desktop "+
				"keep the bridge inside the VM, while `sbx serve` runs out here. Use "+
				"`egress: \"deny\"` for no egress at all, which needs no proxy, or run "+
				"the daemon on a host whose docker is native.",
			gw, err)
	}

	return probe.Close()
}
