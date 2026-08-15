package provider

// The Docker provider: sandboxes as containers on one machine.
//
// Addressing here is the awkward case. Every sandbox shares one loopback, so each gets a
// block of 20 ports and every service is remapped into it — twice, because the daemon has
// to hold a port that answers while the container behind it is stopped:
//
//	client ──▶ 20002 (wake, sbx serve listens)  ──▶ 30002 (backing, docker publishes)
//
// None of that survives leaving the machine, and none of it needs to: see the Kubernetes
// provider, where a pod has its own address and MySQL is just :3306.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/aryanmehrotra/sbx/internal/spec"
	"sync"
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

	mu     sync.Mutex
	health map[string]string // ref -> declared health command
}

func newDocker(ep dockerEndpoint) *dockerProvider {
	return &dockerProvider{api: newDockerClient(ep), endpoint: ep, health: map[string]string{}}
}

func (d *dockerProvider) Name() string { return "docker" }

// Creation shells out to `docker` while the daemon's hot path uses the Engine API. That
// split is deliberate: creation happens once and wants to be readable — you can copy the
// command out of an error and run it — whereas start, stop and inspect happen on every
// wake and want no fork.
// Both halves are pinned to the same resolved endpoint through DOCKER_HOST. Letting the CLI
// do its own resolution would mean `create` could reach one daemon and the wake another, on
// any machine with more than one context — which is most of them.
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
		if !used[i] {
			return i, nil
		}
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

	for i := range eps {
		args = append(args, "-p", fmt.Sprintf("127.0.0.1:%s:%d", backing[i], svc.Ports[i]))
	}

	if svc.Health != "" {
		// The interval is the floor on how long a wake appears to take, because docker only
		// re-evaluates health on it. The long start period is the opposite of low retries:
		// inside it a failing check does not latch the container as unhealthy, while a
		// passing one still flips it immediately — so a database that needs six seconds to
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
// about 150ms, and the command is the one the spec declared — so this is faster without
// being a different question.
func (d *dockerProvider) Probe(ctx context.Context, ref string) (bool, bool) {
	cmd, ok := d.healthCommand(ref)
	if !ok {
		return false, false
	}

	if _, err := d.Exec(ctx, ref, []string{"sh", "-c", cmd}); err != nil {
		return false, true
	}

	return true, true
}

// healthCommand reads the declared check off the container, cached: it cannot change
// without the container being recreated.
func (d *dockerProvider) healthCommand(ref string) (string, bool) {
	d.mu.Lock()
	if cmd, ok := d.health[ref]; ok {
		d.mu.Unlock()
		return cmd, cmd != ""
	}
	d.mu.Unlock()

	raw, err := d.docker("inspect", ref, "--format", "{{json .Config.Healthcheck.Test}}")

	cmd := ""

	if err == nil {
		var test []string
		if json.Unmarshal([]byte(raw), &test) == nil && len(test) > 1 {
			// ["CMD-SHELL", "redis-cli ping"] or ["CMD", "redis-cli", "ping"].
			switch test[0] {
			case "CMD-SHELL":
				cmd = test[1]
			case "CMD":
				cmd = strings.Join(test[1:], " ")
			}
		}
	}

	d.mu.Lock()
	d.health[ref] = cmd
	d.mu.Unlock()

	return cmd, cmd != ""
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

func (d *dockerProvider) List(_ context.Context, sandbox string) ([]Unit, error) {
	filter := "label=" + labelSandbox
	if sandbox != "" {
		filter += "=" + sandbox
	}

	out, err := d.docker("ps", "-aq", "--filter", filter)
	if err != nil {
		// Returning (nil, nil) here reported "no sandboxes" whenever docker was unreachable,
		// which is the same lie as a collector that cannot run and exits 0. An unreachable
		// daemon is an error, not an empty list.
		return nil, fmt.Errorf("listing sandboxes via %s: %w", d.endpoint, err)
	}

	if strings.TrimSpace(out) == "" {
		return nil, nil
	}

	var units []Unit

	for _, id := range strings.Fields(out) {
		f, err := d.docker("inspect", id, "--format",
			label(labelSandbox)+"\t"+label(labelService)+"\t"+label(labelSlot)+"\t"+label(labelPorts)+"\t{{.State.Running}}\t{{.Name}}")
		if err != nil {
			continue
		}

		p := strings.Split(f, "\t")
		if len(p) != 6 {
			continue
		}

		slot, _ := strconv.Atoi(p[2])

		pairs, err := ParsePorts(p[3])
		if err != nil || len(pairs) == 0 {
			continue
		}

		u := Unit{
			Sandbox: p[0],
			Service: p[1],
			Slot:    slot,
			Ref:     strings.TrimPrefix(p[5], "/"),
			Running: p[4] == "true",
			Index:   (pairs[0].Public - publicBase) % blockSize,
		}

		for _, pr := range pairs {
			u.Client = append(u.Client, Endpoint{Host: "127.0.0.1", Port: pr.Public})
			u.Listen = append(u.Listen, pr.Public)
			u.Upstream = append(u.Upstream, Endpoint{Host: "127.0.0.1", Port: pr.Backing})
		}

		units = append(units, u)
	}

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

	return nil
}
