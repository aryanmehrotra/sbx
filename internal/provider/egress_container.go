package provider

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
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

// labelEgressPorts is the port grants a filter container was started with (egress.FormatPortGrants),
// beside labelEgressPolicy: together they are its declaration, and a change to either replaces it.
const labelEgressPorts = "sbx.egress.ports"

// filterSetup is what a service needs to know about the container filter it goes out through.
type filterSetup struct {
	// stat is the loopback address the filter's activity and control endpoint is published on,
	// for the daemon; "" on a remote docker.
	stat string

	// ip is the filter's address on the sandbox's bridge. Fixed (see filterIP), so a service can
	// be told it once, at create, and it stays true when the filter is replaced.
	ip string
}

// ensureFilterContainer starts (or reuses) the sandbox's filter container.
//
// declared and ports are the sandbox's declaration: the policy its spec gives it and the extra
// ports its egress_allow entries grant. A changed declaration replaces the container rather than
// leaving it enforcing the old one - the failure that would otherwise be silent is a host you just
// removed from the spec still being reachable. It is asked on every create, including one over a
// sandbox whose services already exist, because that is exactly when a spec edit arrives.
//
// A policy changed LIVE is not a changed declaration: it is held by the running filter (and by the
// daemon, which pushes it back if the container is replaced), so reusing the container keeps it.
// A replaced declaration drops it: the daemon's copy was made against the old declaration, and
// EgressControl.Sync forgets a copy whose declaration no longer matches.
func (d *dockerProvider) ensureFilterContainer(sandbox string, declared egress.Policy, ports []egress.PortGrant) (filterSetup, error) {
	if err := d.ensureEgressNetwork(sandbox); err != nil {
		return filterSetup{}, err
	}

	body, err := json.Marshal(declared)
	if err != nil {
		return filterSetup{}, err
	}

	list, grants := string(body), egress.FormatPortGrants(ports)
	name, network := filterContainer(sandbox), egressNetwork(sandbox)

	ip, err := d.filterIP(sandbox)
	if err != nil {
		return filterSetup{}, err
	}

	// One inspect for the declaration and the address. The address is the one the container asked
	// for (IPAMConfig), which a stopped container still has, rather than the one it holds now.
	format := label(labelEgressPolicy) + "\x1f" + label(labelEgressPorts) + "\x1f" +
		`{{with index .NetworkSettings.Networks "` + network + `"}}{{with .IPAMConfig}}{{.IPv4Address}}{{end}}{{end}}`

	if cur, err := d.docker("inspect", "--format", format, name); err == nil {
		f := strings.Split(strings.TrimSpace(cur), "\x1f")
		for len(f) < 3 {
			f = append(f, "")
		}

		var why string

		switch {
		case f[0] != list:
			why = "its declared egress policy changed; live changes made with `sbx egress` were dropped"
		case f[1] != grants:
			why = "the ports its egress_allow grants changed"
		case f[2] != ip:
			why = "it predates the filter's fixed address on the sandbox network"
		}

		if why == "" {
			// Already running the right declaration. Make sure it is up: a machine that
			// rebooted leaves it created and stopped.
			if _, err := d.docker("start", name); err == nil {
				stat, err := d.filterStatAddr(name)
				return filterSetup{stat: stat, ip: ip}, err
			}

			why = "it would not start"
		}

		_, _ = d.docker("rm", "-f", name)
		fmt.Printf("  replacing the egress filter for %s: %s\n", sandbox, why)
	}

	image, err := d.ensureFilterImage()
	if err != nil {
		return filterSetup{}, err
	}

	token, err := newToken()
	if err != nil {
		return filterSetup{}, err
	}

	doors, err := d.engineDoors(sandbox)
	if err != nil {
		return filterSetup{}, err
	}

	// The token travels as an environment variable rather than an argument so it is not in
	// the process list of the VM, and as a label so this side can read it back to talk to the
	// container later. Both are visible to whoever can run `docker inspect` - who can already
	// do anything to the container, so that is not a boundary this could defend.
	args := []string{
		"run", "-d", "--name", name,
		"--label", labelSandbox + "=" + sandbox,
		"--label", labelEgressPolicy + "=" + list,
		"--label", labelEgressPorts + "=" + grants,
		"--label", labelEgressToken + "=" + token,
		"-e", "SBX_EGRESS_TOKEN=" + token,
		"--network", network,
		"--ip", ip,
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
		"-ports", grants,
		"-refuse", doors,
		"-listen", ":"+strconv.Itoa(EgressProxyPort),
		"-stat", ":"+strconv.Itoa(filterStatPort),
	)

	if _, err := d.docker(args...); err != nil {
		return filterSetup{}, fmt.Errorf("the egress filter container could not be started: %w", err)
	}

	// The second home. The sandbox's own bridge has masquerade off, which is what denies the
	// workload a route out; the filter needs one, and this is where it gets it. Attached after
	// creation because a container is created on exactly one network.
	//
	// It is docker's default bridge rather than a network of the filter's own because docker's
	// address pools hold about thirty bridges, and a second per filtered sandbox would halve how
	// many sandboxes fit. The price is neighbours: every other container on the default bridge,
	// and the bridge's gateway (the VM). engineDoors closes the whole subnet to the filter, which
	// needs the bridge only as a route out.
	if _, err := d.docker("network", "connect", "bridge", name); err != nil {
		_, _ = d.docker("rm", "-f", name)

		return filterSetup{}, fmt.Errorf("the egress filter has no way out (could not attach it to the "+
			"default bridge): %w", err)
	}

	stat, err := d.filterStatAddr(name)

	return filterSetup{stat: stat, ip: ip}, err
}

// filterIP is the filter's fixed address on the sandbox's bridge: the last usable address of the
// bridge's subnet.
//
// Fixed, because a service reaches the filter by a hosts entry written when the service is created
// (see Create): gVisor's netstack does not use docker's embedded DNS, so under --isolation gvisor
// the network alias never resolved and every request failed with "bad address 'sbx-egress:20999'".
// An address docker assigns changes when the filter is recreated, and would strand that entry.
// The top of the subnet, because docker's IPAM hands addresses out from the bottom, and `--ip`
// reserves it for as long as the filter exists.
func (d *dockerProvider) filterIP(sandbox string) (string, error) {
	out, err := d.docker("network", "inspect", egressNetwork(sandbox),
		"--format", "{{(index .IPAM.Config 0).Subnet}}")
	if err != nil {
		return "", fmt.Errorf("finding the subnet of %s for its egress filter: %w", egressNetwork(sandbox), err)
	}

	ip, err := lastUsable(strings.TrimSpace(out))
	if err != nil {
		return "", fmt.Errorf("the egress filter needs a fixed address on %s: %w", egressNetwork(sandbox), err)
	}

	return ip, nil
}

// lastUsable is the highest address of an IPv4 subnet below its broadcast address.
func lastUsable(subnet string) (string, error) {
	p, err := netip.ParsePrefix(subnet)
	if err != nil {
		return "", fmt.Errorf("%q is not a subnet", subnet)
	}

	if !p.Addr().Is4() || p.Bits() > 30 {
		return "", fmt.Errorf("subnet %s is not an IPv4 subnet with room for a filter "+
			"(docker's default address pools are)", subnet)
	}

	b := p.Masked().Addr().As4()
	n := binary.BigEndian.Uint32(b[:]) | (1<<(32-p.Bits()) - 1) // broadcast
	binary.BigEndian.PutUint32(b[:], n-1)

	return netip.AddrFrom4(b).String(), nil
}

// engineDoors is the -refuse list of a new filter container: the gateway of every network on the
// engine and the default bridge's subnet, plus the sandbox's own gateway. On a VM-backed docker
// every gateway is an address of the VM itself (egress.Doors has the measurements). The container
// adds its own routes and what host.docker.internal and friends resolve to.
func (d *dockerProvider) engineDoors(sandbox string) (string, error) {
	ids, err := d.docker("network", "ls", "-q")
	if err != nil {
		return "", fmt.Errorf("listing docker's networks, to close the egress filter off from them: %w", err)
	}

	format := `{{range .IPAM.Config}}{{if .Gateway}}{{.Gateway}} {{end}}{{end}}` +
		`{{if eq .Name "bridge"}}{{range .IPAM.Config}}{{.Subnet}} {{end}}{{end}}`

	var out []string

	inspect := func(names ...string) error {
		o, err := d.docker(append([]string{"network", "inspect", "--format", format}, names...)...)
		if err == nil {
			out = append(out, strings.Fields(o)...)
		}

		return err
	}

	// All at once; one at a time only when a network vanished between the list and the inspect.
	if ids := strings.Fields(ids); len(ids) > 0 && inspect(ids...) != nil {
		for _, id := range ids {
			_ = inspect(id)
		}
	}

	gw, err := d.egressGateway(sandbox)
	if err != nil {
		return "", err
	}

	out = append(out, gw)

	// Checked here rather than left for the container to refuse: a filter that exits on a bad
	// argument is a sandbox with no egress and a restart loop.
	seen := map[string]bool{}

	var list []string

	for _, s := range out {
		if _, err := egress.ParsePrefixes(s); err != nil || seen[s] {
			continue
		}

		seen[s] = true
		list = append(list, s)
	}

	return strings.Join(list, ","), nil
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

// removeFilterContainer takes the sandbox's filter down, and reports whether there was one.
// Called where the sandbox's containers and its network are removed, so a filter never outlives
// what it was filtering for.
func (d *dockerProvider) removeFilterContainer(sandbox string) bool {
	// Asked first because `docker rm -f` exits 0 for a container that does not exist, which
	// made every rm of a sandbox without a filter report removing one.
	if _, err := d.docker("inspect", "-f", "{{.Id}}", filterContainer(sandbox)); err != nil {
		return false
	}

	if _, err := d.docker("rm", "-f", filterContainer(sandbox)); err == nil {
		fmt.Printf("  removed the egress filter for %s\n", sandbox)
		return true
	}

	return false
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

	f, err := filterOf(sandbox, units)
	if err != nil {
		return EgressFilter{}, err
	}

	name := filterContainer(sandbox)

	out, err := d.docker("inspect", "--format", label(labelEgressToken)+"\x1f"+label(labelEgressPolicy), name)
	if err != nil {
		// No container: the filter is the daemon's own listener on the gateway.
		return f, nil
	}

	token, declared, _ := strings.Cut(strings.TrimSpace(out), "\x1f")

	// The filter's own label is the declaration it enforces, and it wins over the services'.
	// `sbx create` run again after a spec edit replaces the filter but cannot relabel a service
	// container that already exists, so the services' labels still name the old declaration - and
	// a reset back to that, or a saved live copy made against it and pushed back, would undo the
	// edit.
	f.Declared = filterDeclaration(f.Declared, declared)

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

// filterOf is the part of finding a sandbox's filter every provider shares: which of its units
// are filtered, what they declared, and the gateway they share.
func filterOf(sandbox string, units []Unit) (EgressFilter, error) {
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

	return f, nil
}

// filterDeclaration is the declaration a sandbox's filter enforces: the filter container's own
// label when it has a readable one, else what the services declared.
func filterDeclaration(services egress.Policy, filterLabel string) egress.Policy {
	if filterLabel == "" {
		return services
	}

	if p, err := egress.ParsePolicy([]byte(filterLabel)); err == nil {
		return p
	}

	return services
}
