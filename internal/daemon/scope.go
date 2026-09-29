package daemon

// Which sandboxes this daemon is allowed to touch.
//
// A daemon adopts every container labelled sbx.sandbox on its docker engine: it binds their
// ports, wakes them on connect and - after --idle - stops them. That is right for the one daemon a
// machine is meant to have, and dangerous for a second one: a test harness, or an OpenSandbox
// conformance run, started beside somebody's live stack would front that stack and then put it to
// sleep underneath them.
//
// `--only` fences it. Outside the scope a sandbox is invisible to this process - never fronted,
// woken, slept, reaped, frozen or removed, and refused by the control API - exactly as if it were
// on another engine. The default is the whole engine, as it always was.

import (
	"context"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"github.com/aryanmehrotra/sbx/internal/logs"
	"github.com/aryanmehrotra/sbx/internal/provider"
)

// Scope is a set of sandbox-name patterns. Empty matches everything.
//
// A pattern with glob characters is a glob (path.Match); one without is a prefix, because the
// case this exists for - `--only osb-` - is a prefix, and making everybody write `osb-*` for it
// would be a trap with no benefit.
type Scope []string

// ParseScope reads --only values, each of which may itself be comma-separated.
func ParseScope(values []string) (Scope, error) {
	var s Scope

	for _, v := range values {
		for _, p := range strings.Split(v, ",") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}

			if _, err := path.Match(p, ""); err != nil {
				return nil, fmt.Errorf("--only %q is not a valid pattern: %v", p, err)
			}

			s = append(s, p)
		}
	}

	return s, nil
}

// Match reports whether a sandbox name is inside the scope.
func (s Scope) Match(sandbox string) bool {
	if len(s) == 0 {
		return true
	}

	for _, p := range s {
		if strings.ContainsAny(p, "*?[") {
			if ok, _ := path.Match(p, sandbox); ok {
				return true
			}

			continue
		}

		if strings.HasPrefix(sandbox, p) {
			return true
		}
	}

	return false
}

func (s Scope) String() string {
	if len(s) == 0 {
		return "all"
	}

	return strings.Join(s, ",")
}

// refInScope reports whether a provider ref belongs to a sandbox this daemon may act on. Only
// refs the daemon adopted qualify when a scope is set: a ref is a container name, and working
// out its sandbox from the name would be a guess the control API must not make.
func (d *daemon) refInScope(ref string) bool {
	if len(d.scope) == 0 && d.servesOSB {
		return true
	}

	d.mu.Lock()
	_, ok := d.units[ref]
	d.mu.Unlock()

	if ok || len(d.scope) > 0 {
		return ok
	}

	// Unscoped, and not the API's daemon: anything but an API sandbox is ours, adopted yet or
	// not. Asked of the provider rather than read from the name, for the reason above.
	return !d.osbOwned(func(u provider.Unit) bool { return u.Ref == ref })
}

// sandboxInScope is refInScope for a sandbox name: the scope, and - for an unscoped daemon that
// does not serve the API - not a sandbox the API created.
func (d *daemon) sandboxInScope(sandbox string) bool {
	if !d.scope.Match(sandbox) {
		return false
	}

	if len(d.scope) > 0 || d.servesOSB {
		return true
	}

	// Adopted is ours by definition: discover already applied adopts to it.
	d.mu.Lock()
	for _, u := range d.units {
		if u.sandbox == sandbox {
			d.mu.Unlock()
			return true
		}
	}
	d.mu.Unlock()

	return !d.osbOwned(func(u provider.Unit) bool { return u.Sandbox == sandbox })
}

// adopts is the one test of whether a discovered unit is this daemon's: inside --only, and not a
// container the OpenSandbox API created unless this daemon serves that API or was scoped to it.
//
// The second half exists because the machine's own daemon is unscoped. Beside a second daemon
// serving --osb-addr - the documented way to run the API next to a live stack - it adopted the
// API's containers too: fronted them on ports the API's daemon also wanted, froze or stopped them
// on its own idle clock, and knew nothing of their API pauses, so it would wake a Paused sandbox
// on the first connection. Now an API sandbox has exactly one daemon.
//
// An unscoped daemon also leaves alone any sandbox a live --only daemon covers. Both adopted it
// before, so two daemons raced to bind its ports: the loser logged "address already in use"
// every tick, and which one fronted the sandbox was an accident of timing - measured, the
// machine's daemon took the ports in the sub-second gap while a scoped one restarted.
func (d *daemon) adopts(u provider.Unit) bool { return d.adoptsGiven(u, d.scopedClaims()) }

// adoptsGiven is adopts against a registry already read, so a discovery pass reads it once.
func (d *daemon) adoptsGiven(u provider.Unit, claims []Presence) bool {
	if !d.scope.Match(u.Sandbox) {
		return false
	}

	if len(d.scope) == 0 {
		for _, p := range claims {
			if p.Scope.Match(u.Sandbox) {
				d.noteDeferred(u.Sandbox, p.PID)
				return false
			}
		}

		d.noteDeferred(u.Sandbox, 0)
	}

	return u.OSB == "" || d.servesOSB || len(d.scope) > 0
}

// scopedClaims is the live --only daemons whose sandboxes an unscoped daemon leaves to them.
//
// Read from the registry every discovery pass rather than once at start, so a scoped daemon
// that starts later takes its sandboxes over on this daemon's next tick, and one that stops
// hands them back on the next. Liveness is the pid, checked by scopedDaemons, which also deletes
// a dead daemon's record: a killed --only daemon must not hide its sandboxes for good.
//
// A scoped daemon reads nothing. Its --only is what it asked for, and overlap between two scoped
// daemons is the operator's to sort out.
func (d *daemon) scopedClaims() []Presence {
	if len(d.scope) > 0 {
		return nil
	}

	var out []Presence

	for _, p := range scopedRegistry() {
		if p.PID != os.Getpid() {
			out = append(out, p)
		}
	}

	return out
}

// scopedRegistry is scopedDaemons, a variable so the package's tests do not read the real
// ~/.sbx/daemons: a developer's own --only daemon would otherwise hide fixtures from them.
var scopedRegistry = scopedDaemons

// noteDeferred says so when a sandbox passes to, or back from, a scoped daemon - once per change,
// not every pass. pid 0 means this daemon fronts it.
func (d *daemon) noteDeferred(sandbox string, pid int) {
	d.mu.Lock()
	was := d.deferred[sandbox]

	if pid == was {
		d.mu.Unlock()
		return
	}

	if d.deferred == nil {
		d.deferred = map[string]int{}
	}

	if pid == 0 {
		delete(d.deferred, sandbox)
	} else {
		d.deferred[sandbox] = pid
	}
	d.mu.Unlock()

	if pid != 0 {
		logs.Default.Info(sandbox, "", "left to the --only daemon (pid %d) that covers it", pid)
	} else {
		logs.Default.Info(sandbox, "", "fronting again: the --only daemon (pid %d) that covered it has stopped", was)
	}
}

// osbOwned reports whether any unit matching pick is an API sandbox. A provider that cannot be
// asked answers "yes": refusing a control call is recoverable, acting on another daemon's
// sandbox is not.
func (d *daemon) osbOwned(pick func(provider.Unit) bool) bool {
	if d.provider == nil {
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	units, err := d.provider.List(ctx, "")
	if err != nil {
		return true
	}

	for _, u := range units {
		if pick(u) && u.OSB != "" {
			return true
		}
	}

	return false
}

// stringList is a repeatable flag.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

func outOfScope(what string, s Scope) string {
	return fmt.Sprintf("%s is outside this daemon's --only %s, so it will not act on it - use the "+
		"daemon that owns it, or the local CLI", what, s)
}

// SetScope fences a daemon built with New, as --only does. Selftest uses it so the daemon it runs
// in-process beside the machine's own `sbx serve` touches its own sandbox and nothing else.
func (d *daemon) SetScope(s Scope) { d.scope = s }

// Exact is a scope of one sandbox name and nothing else.
//
// A bare pattern is a prefix (see Scope), so scoping to "selftest-42" would also take in
// "selftest-421" - another selftest run, which this daemon would then front and sleep. Bracketing
// the first character makes the pattern a glob, and a glob matches the whole name only. A sandbox
// name starts with a letter or digit and holds no glob characters (cli.ValidateName), so nothing
// else needs escaping.
func Exact(name string) Scope {
	if name == "" {
		return Scope{"/"} // a prefix no sandbox name can start with: an empty name matches nothing
	}

	return Scope{"[" + name[:1] + "]" + name[1:]}
}
