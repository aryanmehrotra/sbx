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
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

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
	if svc.Egress == spec.EgressDeny {
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

	if svc.GPUs != "" {
		args = append(args, "--gpus", svc.GPUs)
	}

	if svc.Egress == spec.EgressDeny {
		args = append(args, "--network", egressNetwork(sandbox))
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
			"--health-interval", "300ms",
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

	args = append(args, svc.Image)
	args = append(args, svc.Args...)

	_, err := d.docker(args...)

	return err
}

func egressNetwork(sandbox string) string { return "sbx-noegress-" + sandbox }

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
		// Returning (nil, nil) here reported "no sandboxes" whenever docker was unreachable,
		// which is the same lie as a collector that cannot run and exits 0. An unreachable
		// daemon is an error, not an empty list.
		return nil, fmt.Errorf("listing sandboxes via %s: %w", d.endpoint, err)
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
			Sandbox: c.Labels[labelSandbox],
			Service: c.Labels[labelService],
			Slot:    slot,
			Ref:     c.name(),
			Running: c.State == "running",
			Index:   (pairs[0].Public - publicBase) % blockSize,
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
