package cli

// The commands. Everything here is provider-agnostic: it decides *what* a sandbox is and
// *when* it is serving, and asks a Provider where that lives.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aryanmehrotra/sbx/internal/daemon"
	"github.com/aryanmehrotra/sbx/internal/logs"
	"github.com/aryanmehrotra/sbx/internal/provider"
	"github.com/aryanmehrotra/sbx/internal/spec"
)

// ── create ───────────────────────────────────────────────────────────────────

func Create(ctx context.Context, p provider.Provider, path, sandbox string, withOptional bool, iso provider.Isolation) error {
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

	skipped := map[string]bool{}

	for _, name := range order {
		svc := sp.Services[name]

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

		if err := createOne(ctx, p, sandbox, slot, start, name, svc, specDir, iso); err != nil {
			return err
		}

		// The slot now belongs to a real container, so nothing else can be handed it. Every
		// remaining service - pulls, health checks, init - proceeds unserialised.
		releaseSlot()

		created = append(created, p.Endpoints(sandbox, name, slot, start, svc.Ports)...)
	}

	fmt.Println()
	fmt.Println(readiness(created))

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
func readiness(eps []provider.Endpoint) string {
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
	if _, ok := daemon.Running(); !ok {
		return "no `sbx serve` is running, so nothing accepts on the ports `sbx env` exports.\n" +
			"Start one - once per machine, not once per sandbox:\n\n" +
			"    sbx serve --idle 5m &\n\n" +
			"deploy/ has a launchd plist and a systemd unit for running it supervised."
	}

	// There is a daemon, and it finds new sandboxes on its refresh tick - so for a moment
	// after create, the exported ports are still dead. Waiting here rather than handing back
	// an address that is about to work is the difference between the README's three-line
	// quickstart being true and being true-eventually.
	if waitReachable(local, 30*time.Second) {
		return "ready. Nothing needs starting again - connecting wakes it, idleness sleeps it."
	}

	return "created, but the running `sbx serve` has not picked it up yet.\n" +
		"It looks for new sandboxes on its --refresh interval; give it one, or restart it."
}

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
	eps := p.Endpoints(sandbox, name, slot, start, svc.Ports)

	if err := p.Create(ctx, sandbox, slot, start, name, svc, eps, specDir, iso); err != nil {
		return fmt.Errorf("service %q: %w", name, err)
	}

	ref, err := refFor(ctx, p, sandbox, name)
	if err != nil {
		return err
	}

	if err := checkMounts(ctx, p, ref, name, svc, specDir); err != nil {
		return err
	}

	if svc.Health != "" {
		if err := waitHealthy(ctx, p, ref, svc.Health, 120*time.Second); err != nil {
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
	units, err := p.List(ctx, sandbox)
	if err != nil {
		return "", err
	}

	for _, u := range units {
		if u.Service == service {
			return u.Ref, nil
		}
	}

	return "", fmt.Errorf("service %q was created but the provider does not list it", service)
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
	// it kept returning it.
	if command != "" {
		if out, err := p.Exec(ctx, ref, []string{"sh", "-c", command}); err != nil {
			return fmt.Errorf("%s never became ready within %s - the health command %q still "+
				"fails: %s", ref, timeout, command, firstLine(out))
		}
	}

	return fmt.Errorf("%s never became ready within %s", ref, timeout)
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

	switch {
	case strings.Contains(text, "exit status 127"), strings.Contains(text, "not found"):
		return detail, true
	case strings.Contains(text, "exit status 126"), strings.Contains(text, "Permission denied"):
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

	if sp, err := spec.LoadSpec(specPath); err == nil {
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

func Env(ctx context.Context, p provider.Provider, path, sandbox, shell string) error {
	// The sandbox is checked first, on purpose. Loading the spec first meant that `sbx env
	// typo-x` in a directory with no sandbox.json reported the missing spec - so the reader
	// was told to write a spec when what they had actually done was mistype a name.
	units, err := p.List(ctx, sandbox)
	if err != nil {
		return err
	}

	if len(units) == 0 {
		return UnknownSandbox(ctx, p, sandbox)
	}

	sp, err := spec.LoadSpec(path)
	if err != nil {
		return err
	}

	layout, err := sp.Assign()
	if err != nil {
		return err
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
			return fmt.Errorf("export %s: %s is not assigned an endpoint", env, sp.Exports[env])
		}

		vars = append(vars,
			[2]string{hostVar(env), ep.Host},
			[2]string{env, strconv.Itoa(ep.Port)},
		)
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

// hostVar is the companion variable for a declared port export.
//
// `DATABASE_PORT` gets `DATABASE_HOST`, which is the convention most application config
// already reads. `PGPORT` gets `PGHOST` - no underscore - because that is what libpq itself
// reads, and it is the difference between the README's `psql` example working and not: with
// PGHOST and PGPORT set, `psql` with no arguments connects to the sandbox. The same shape
// covers MYSQL_HOST/MYSQL_PORT and REDIS_HOST/REDIS_PORT without special-casing any of them.
func hostVar(portVar string) string {
	if base := strings.TrimSuffix(portVar, "_PORT"); base != portVar && base != "" {
		return base + "_HOST"
	}

	// PGPORT → PGHOST. Guarded on a non-empty base so a bare "PORT" export does not become
	// "_HOST", which would be neither useful nor obviously wrong to whoever wrote it.
	if base := strings.TrimSuffix(portVar, "PORT"); base != portVar && base != "" {
		return base + "HOST"
	}

	return portVar + "_HOST"
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

func serviceRef(ctx context.Context, p provider.Provider, sandbox, service string) (string, error) {
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
				if err := p.Start(ctx, u.Ref); err != nil {
					return "", err
				}

				if err := waitHealthy(ctx, p, u.Ref, "", 90*time.Second); err != nil {
					return "", err
				}
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

// Exec runs a command inside a service. With tty it hands the terminal over instead of
// capturing output, which is what makes `sbx exec -t my-branch postgres psql` a usable
// shell rather than a command that appears to hang with no prompt.
func Exec(ctx context.Context, p provider.Provider, sandbox, service string, argv []string, tty bool) error {
	ref, err := serviceRef(ctx, p, sandbox, service)
	if err != nil {
		return err
	}

	if tty {
		return p.ExecTTY(ctx, ref, argv)
	}

	out, err := p.Exec(ctx, ref, argv)
	if out != "" {
		fmt.Println(out)
	}

	return err
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

		return p.Logs(ctx, ref, lines, follow, w)
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
			}
		}(u)
	}

	wg.Wait()

	return errors.Join(errs...)
}

func Copy(ctx context.Context, p provider.Provider, sandbox, service, src, dst string) error {
	if strings.HasPrefix(src, ":") == strings.HasPrefix(dst, ":") {
		return fmt.Errorf("exactly one of src and dst must be inside the sandbox, written as \":path\"")
	}

	ref, err := serviceRef(ctx, p, sandbox, service)
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

	fmt.Printf("%-20s %-14s %-9s %s\n", "SANDBOX", "SERVICE", "STATE", "ADDRESS")

	for _, u := range units {
		state := "asleep"
		if u.Running {
			state = "awake"
		}

		fmt.Printf("%-20s %-14s %-9s %s\n", u.Sandbox, u.Service, state, joinEndpoints(u.Client))
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
			Sandbox: u.Sandbox, Service: u.Service, Awake: u.Running,
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

	fmt.Printf("sandbox %q destroyed\n", sandbox)

	return nil
}
