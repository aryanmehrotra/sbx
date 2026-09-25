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
	"fmt"
	"path"
	"strings"
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
	if len(d.scope) == 0 {
		return true
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	_, ok := d.units[ref]

	return ok
}

// stringList is a repeatable flag.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

func outOfScope(what string, s Scope) string {
	return fmt.Sprintf("%s is outside this daemon's --only %s, so it will not act on it - use the "+
		"daemon that owns it, or the local CLI", what, s)
}
