package provider

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/aryanmehrotra/sbx/internal/egress"
)

// The egress filter, run as a container on the sandbox's own bridge instead of as a listener the
// daemon opens on the bridge gateway.
//
// The listener is the better arrangement where it works: no image to build, no second process,
// and the filter is in the daemon that already knows which units are awake. It only works where
// the bridge is on the same machine as `sbx serve`. On colima, Docker Desktop, rootless docker
// and a remote DOCKER_HOST it is not, and `egress_allow` was refused outright there - which is
// every Mac, and so most of the people the feature was written for.
//
// A container is on the right side of that line by construction: it runs where the bridge is.
//
// It is dual-homed on purpose - the sandbox's no-NAT bridge, where the workload can reach it,
// and an ordinary bridge, where it can reach the internet. That is a bastion, and it keeps the
// property the whole feature rests on: the workload itself still has no route out, so a client
// that ignores HTTP_PROXY and dials a host directly gets nowhere. The filter is the only door,
// as before; what changed is which side of the VM boundary the door is on.
const (
	// filterBuilderImage compiles the filter; filterRuntimeImage carries it. Both are pinned by
	// scripts/pin-templates.sh along with the template images, so an unattended `docker build`
	// months from now produces the binary that was tested rather than whatever moved under the
	// tag. Alpine rather than scratch for the runtime: the binary is static and does not need
	// it, but a filter you cannot get a shell into is a filter you cannot diagnose.
	filterBuilderImage = "golang:1.26-alpine"
	filterRuntimeImage = "alpine:3.20"

	// filterStatPort is where the container reports its last activity, for the daemon to scrape.
	// Published to loopback on a port docker picks, so it cannot collide with the block the
	// daemon hands out to sandboxes.
	filterStatPort = 20998

	// filterAlias is the name the workload's HTTP_PROXY points at. A name, not an address: the
	// container's IP on the bridge is assigned at start and changes when it is recreated, and
	// the whole point of the alias is that the env var can be written before it exists.
	filterAlias = "sbx-egress"
)

// filterContainer is the name of a sandbox's filter container.
func filterContainer(sandbox string) string { return "sbx-egressfilter-" + sandbox }

// filterImageTag names the image built from a given build context. The tag is the context's own
// hash, so a filter whose source changed gets a different image and is rebuilt, and one whose
// source did not is found in the cache and is not.
func filterImageTag(files map[string]string) string {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}

	sort.Strings(names)

	h := sha256.New()
	for _, n := range names {
		fmt.Fprintf(h, "%s\x00%s\x00", n, files[n])
	}

	return "sbx-egress-filter:" + hex.EncodeToString(h.Sum(nil))[:16]
}

// ensureFilterImage builds the filter image if this machine does not already have it, and
// returns its tag.
func (d *dockerProvider) ensureFilterImage() (string, error) {
	files, err := egress.BuildContext(filterBuilderImage, filterRuntimeImage)
	if err != nil {
		return "", err
	}

	tag := filterImageTag(files)

	if _, err := d.docker("image", "inspect", tag); err == nil {
		return tag, nil
	}

	dir, err := os.MkdirTemp("", "sbx-egress-*")
	if err != nil {
		return "", err
	}

	defer os.RemoveAll(dir)

	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			return "", err
		}
	}

	// Said out loud because it is the one slow step: a compiler image to pull and a build to
	// run, once per machine per change to the filter. Silence here reads as a hang.
	fmt.Printf("  building the egress filter image (once per machine)...\n")

	if _, err := d.docker("build", "-t", tag, dir); err != nil {
		return "", fmt.Errorf("the egress filter image could not be built: %w", err)
	}

	return tag, nil
}

// ensureFilterContainer starts (or reuses) the sandbox's filter container and returns the
// loopback address its stat endpoint is published on, for the daemon to scrape.
//
// declared is the policy the spec gives the sandbox. A changed declaration replaces the
// container rather than leaving it enforcing the old one - the failure that would otherwise be
// silent is a host you just removed from the spec still being reachable. A policy changed LIVE
// is not a changed declaration: it is held by the running filter (and by the daemon, which
// pushes it back if the container is ever replaced), so reusing the container keeps it.
func (d *dockerProvider) ensureFilterContainer(sandbox string, declared egress.Policy) (string, error) {
	if err := d.ensureEgressNetwork(sandbox); err != nil {
		return "", err
	}

	body, err := json.Marshal(declared)
	if err != nil {
		return "", err
	}

	list := string(body)
	name := filterContainer(sandbox)

	if cur, err := d.docker("inspect", "--format", "{{index .Config.Labels \""+labelEgressPolicy+"\"}}", name); err == nil {
		if strings.TrimSpace(cur) == list {
			// Already running the right list. Make sure it is up: a machine that rebooted
			// leaves it created and stopped.
			if _, err := d.docker("start", name); err == nil {
				return d.filterStatAddr(name)
			}
		}

		_, _ = d.docker("rm", "-f", name)
	}

	image, err := d.ensureFilterImage()
	if err != nil {
		return "", err
	}

	token, err := newToken()
	if err != nil {
		return "", err
	}

	// The token travels as an environment variable rather than an argument so it is not in
	// the process list of the VM, and as a label so this side can read it back to talk to the
	// container later. Both are visible to whoever can run `docker inspect` - who can already
	// do anything to the container, so that is not a boundary this could defend.
	args := []string{
		"run", "-d", "--name", name,
		"--label", labelSandbox + "=" + sandbox,
		"--label", labelEgressPolicy + "=" + list,
		"--label", labelEgressToken + "=" + token,
		"-e", "SBX_EGRESS_TOKEN=" + token,
		"--network", egressNetwork(sandbox),
		"--network-alias", filterAlias,
		"--restart", "unless-stopped",
	}

	// The activity endpoint is published to loopback for the daemon to scrape - but against a
	// remote dockerd that is the REMOTE machine's loopback, and the reading would never arrive.
	// Filtering is unaffected either way; only the idle signal is, so the port is simply not
	// published there rather than the whole feature being refused for it.
	if d.endpoint.Local() {
		// Docker picks the host port, so this cannot collide with the daemon's own block.
		args = append(args, "-p", "127.0.0.1::"+strconv.Itoa(filterStatPort))
	}

	args = append(args,
		image,
		"-policy", list,
		"-listen", ":"+strconv.Itoa(EgressProxyPort),
		"-stat", ":"+strconv.Itoa(filterStatPort),
	)

	if _, err := d.docker(args...); err != nil {
		return "", fmt.Errorf("the egress filter container could not be started: %w", err)
	}

	// The second home. The sandbox's own bridge has masquerade off, which is what denies the
	// workload a route out; the filter needs one, and this is where it gets it. Attached after
	// creation because a container is created on exactly one network.
	if _, err := d.docker("network", "connect", "bridge", name); err != nil {
		_, _ = d.docker("rm", "-f", name)

		return "", fmt.Errorf("the egress filter has no way out (could not attach it to the "+
			"default bridge): %w", err)
	}

	return d.filterStatAddr(name)
}

// filterStatAddr asks docker which loopback address it published the stat port on, or returns ""
// when it was deliberately not published - a remote dockerd, where the reading could not reach
// us. Empty is a working filter without an idle signal, not a failure, so it is not an error.
func (d *dockerProvider) filterStatAddr(name string) (string, error) {
	if !d.endpoint.Local() {
		return "", nil
	}

	out, err := d.docker("port", name, strconv.Itoa(filterStatPort)+"/tcp")
	if err != nil {
		return "", err
	}

	// "127.0.0.1:54321" - and on some engines a second line for ::1, which is the same port.
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, "127.0.0.1:") {
			return line, nil
		}
	}

	return "", fmt.Errorf("the egress filter's stat port is not published on loopback: %q", out)
}

// removeFilterContainer takes the sandbox's filter down. Called where the sandbox's containers
// and its network are removed, so a filter never outlives what it was filtering for.
func (d *dockerProvider) removeFilterContainer(sandbox string) {
	if _, err := d.docker("rm", "-f", filterContainer(sandbox)); err == nil {
		fmt.Printf("  removed the egress filter for %s\n", sandbox)
	}
}

func newToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("could not generate the egress filter's control token: %w", err)
	}

	return hex.EncodeToString(b), nil
}

// EgressFilter is what it takes to change a sandbox's egress policy while it runs.
type EgressFilter struct {
	Sandbox string
	Gateway string

	// Services are the sandbox's filtered services. They share this one filter.
	Services []string

	// Declared is the policy the spec gave the sandbox: what the filter was created with, and
	// what a reset returns to.
	Declared egress.Policy

	// Control is the loopback address of a container filter's control endpoint, and Token the
	// secret it requires. Both empty when the filter is a listener inside the daemon itself.
	Control string
	Token   string
}

// EgressFilters is a provider that can find a sandbox's egress filter. Optional: a provider
// without one has no filter whose policy could be changed.
type EgressFilters interface {
	EgressFilter(ctx context.Context, sandbox string) (EgressFilter, error)
}

// ErrNoSandbox and ErrNotFiltered are the two ways there is no filter to talk to, told apart
// because they need different answers: a 404, and "recreate it with a policy".
var (
	ErrNoSandbox   = errors.New("no such sandbox")
	ErrNotFiltered = errors.New("no egress filter")
)

// DeclaredPolicy is the policy a sandbox's filtered units declare. Units created before
// policies existed carry only an allow-list, and their union is what those always meant.
func DeclaredPolicy(units []Unit) egress.Policy {
	var allow []string

	for _, u := range units {
		if u.EgressPolicy != "" {
			if p, err := egress.ParsePolicy([]byte(u.EgressPolicy)); err == nil {
				return p
			}

			// A label this build cannot read is refused the only safe way.
			return egress.DenyAll()
		}

		allow = append(allow, u.EgressAllow...)
	}

	sort.Strings(allow)

	return egress.FromAllowList(allow)
}

func (d *dockerProvider) EgressFilter(ctx context.Context, sandbox string) (EgressFilter, error) {
	units, err := d.List(ctx, sandbox)
	if err != nil {
		return EgressFilter{}, err
	}

	if len(units) == 0 {
		return EgressFilter{}, fmt.Errorf("%w %q", ErrNoSandbox, sandbox)
	}

	f := EgressFilter{Sandbox: sandbox}

	var filtered []Unit

	for _, u := range units {
		if u.EgressGateway != "" {
			filtered = append(filtered, u)
			f.Services = append(f.Services, u.Service)
			f.Gateway = u.EgressGateway
		}
	}

	if len(filtered) == 0 {
		return EgressFilter{}, fmt.Errorf("%w: sandbox %q was created without egress_policy, "+
			"egress_allow or egress: \"allow\", so its traffic does not pass through anything "+
			"sbx can change. Add one of those to the spec and recreate it", ErrNotFiltered, sandbox)
	}

	sort.Strings(f.Services)
	f.Declared = DeclaredPolicy(filtered)

	name := filterContainer(sandbox)

	token, err := d.docker("inspect", "--format", "{{index .Config.Labels \""+labelEgressToken+"\"}}", name)
	if err != nil {
		// No container: the filter is the daemon's own listener on the gateway.
		return f, nil
	}

	if f.Token = strings.TrimSpace(token); f.Token == "" {
		return EgressFilter{}, fmt.Errorf("the egress filter for %q predates live policies and "+
			"has no control token. Recreate the sandbox (sbx rm, then sbx create) to change "+
			"its policy without restarting it from then on", sandbox)
	}

	if !d.endpoint.Local() {
		return EgressFilter{}, fmt.Errorf("the egress filter for %q runs on a remote docker, "+
			"and its control endpoint is published on THAT machine's loopback, not this one's - "+
			"change the policy from the machine docker runs on", sandbox)
	}

	// Asked fresh rather than read from a label: docker picks the published port when the
	// container starts, so a filter restarted by a reboot answers somewhere new.
	if f.Control, err = d.filterStatAddr(name); err != nil {
		return EgressFilter{}, fmt.Errorf("the egress filter for %q is not answering "+
			"(docker start %s brings it back): %w", sandbox, name, err)
	}

	return f, nil
}
