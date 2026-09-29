package cli

// The <SERVICE>_HOST / <SERVICE>_PORT names `sbx env` derives for services no export names, and
// the collisions between them - decided in one place, so `sbx env`, `sbx add`, `sbx validate`
// and `sbx create` cannot disagree about which name goes to whom.

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/aryanmehrotra/sbx/internal/provider"
	"github.com/aryanmehrotra/sbx/internal/spec"
)

// envCandidate is a service that would get derived variables, and the address they would carry.
type envCandidate struct {
	service string
	ep      provider.Endpoint
}

// envCollision is a derived name withheld from services, and the warning that says so.
type envCollision struct {
	services []string
	text     string
}

func (c envCollision) involves(service string) bool { return slices.Contains(c.services, service) }

// exportedNames is every variable `sbx env` sets before deriving any: its own two, each export,
// and each export's host companion. A derived name never replaces one of these.
func exportedNames(sp *spec.Spec) map[string]bool {
	taken := map[string]bool{"SBX_SANDBOX": true, "SBX_PROVIDER": true}

	for env := range sp.Exports {
		taken[env] = true

		if host, ok := hostVar(env); ok {
			taken[host] = true
		}
	}

	return taken
}

// deriveEnvNames hands each candidate <NAME>_HOST and <NAME>_PORT, and reports every name it
// withheld. taken is updated with what it hands out.
//
// Grouped by the name each would take before any is handed out, because a collision has to be
// seen whole: taking them in order gave the name to whichever service sorted first.
func deriveEnvNames(sp *spec.Spec, cands []envCandidate, taken map[string]bool) ([][2]string, []envCollision) {
	sorted := slices.Clone(cands)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].service < sorted[j].service })

	byName := map[string][]envCandidate{}

	var names []string

	for _, c := range sorted {
		base := envName(c.service)
		if n := len(byName[base]); n > 0 && byName[base][n-1].service == c.service {
			continue // one service listed twice is not a collision with itself
		}

		if byName[base] == nil {
			names = append(names, base)
		}

		byName[base] = append(byName[base], c)
	}

	// What to do about it depends on where the service came from. An export can only name a
	// service in sandbox.json - one pointing anywhere else fails `sbx env` outright - so a
	// service from `sbx add` can only be added again under another name.
	fix := func(service string) string {
		if _, inSpec := sp.Services[service]; inSpec {
			return fmt.Sprintf("give %q an `exports` entry in sandbox.json", service)
		}

		return fmt.Sprintf("%q came from `sbx add`, which no export can name: add it again under another name", service)
	}

	var (
		out  [][2]string
		cols []envCollision
	)

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
				cols = append(cols, envCollision{services: []string{c.service}, text: fmt.Sprintf(
					"service %q gets no %s: an export already has that name. To address it, %s",
					c.service, held, fix(c.service))})
			}

		case len(cs) > 1:
			// None of them gets it. Handing it to one means the name points at the wrong
			// service for anyone who meant another, and nothing would say which.
			col := envCollision{}
			quoted, fixes := make([]string, len(cs)), make([]string, len(cs))

			for i, c := range cs {
				col.services = append(col.services, c.service)
				quoted[i], fixes[i] = fmt.Sprintf("%q", c.service), fix(c.service)
			}

			col.text = fmt.Sprintf("services %s map to the same %s and %s, so none of them gets those. To address them, %s",
				strings.Join(quoted, " and "), host, port, strings.Join(fixes, "; "))
			cols = append(cols, col)

		default:
			taken[host], taken[port] = true, true
			out = append(out, [2]string{host, cs[0].ep.Host}, [2]string{port, fmt.Sprint(cs[0].ep.Port)})
		}
	}

	return out, cols
}

// unitCandidates is every unit no export names that has a port to address: its first spec port
// through index where the spec declares it, else the unit's own first client address.
func unitCandidates(sp *spec.Spec, units []provider.Unit, index map[string]provider.Endpoint) []envCandidate {
	exported := exportedServices(sp)

	var out []envCandidate

	for _, u := range units {
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

		if ok { // no ports: nothing to connect to
			out = append(out, envCandidate{u.Service, ep})
		}
	}

	return out
}

func exportedServices(sp *spec.Spec) map[string]bool {
	exported := map[string]bool{}

	for _, ref := range sp.Exports {
		svc, _, _ := strings.Cut(ref, ":")
		exported[svc] = true
	}

	return exported
}

// warnEnvCollisions prints each collision on stderr (see the stderr var). Silence was the bug:
// the service had no variables, and the first sign was a client dialling another.
func warnEnvCollisions(cols []envCollision) {
	for _, c := range cols {
		fmt.Fprintf(stderr, "sbx: warning: %s\n", c.text)
	}
}

// specEnvCollisions is what the spec alone shows: two of its services deriving one name, or an
// export named like a service's derived name. `sbx validate` and `sbx create` warn with it, so a
// spec that withholds a name says so before anybody runs `sbx env` against it.
func specEnvCollisions(sp *spec.Spec) []envCollision {
	exported := exportedServices(sp)

	var cands []envCandidate

	for name, svc := range sp.Services {
		if !exported[name] && len(svc.Ports) > 0 {
			cands = append(cands, envCandidate{service: name})
		}
	}

	_, cols := deriveEnvNames(sp, cands, exportedNames(sp))

	return cols
}

// addEnvCollisions is what adding service to a sandbox of units would withhold, counting only the
// collisions the new service is part of: one already there is `sbx env`'s to report, and
// repeating it on every add teaches people to skip the line.
func addEnvCollisions(sp *spec.Spec, units []provider.Unit, service string) []envCollision {
	cands := append(unitCandidates(sp, units, nil), envCandidate{service: service})

	_, cols := deriveEnvNames(sp, cands, exportedNames(sp))

	var mine []envCollision

	for _, c := range cols {
		if c.involves(service) {
			mine = append(mine, c)
		}
	}

	return mine
}
