package spec

// The order services are created in.
//
// Until `build:` existed this did not matter: everything in a spec was a backing service
// that came up on its own and waited for nobody. Now a service can be your own application,
// and an app that dials its database at boot races a database that is created after it -
// alphabetically, which is to say by accident. `api` before `postgres` fails; `web` after it
// works; nothing in the spec expresses the difference.
//
// Two things this deliberately is not:
//
//   - It is not a port assignment. Ordinals stay alphabetical (see Assign), so adding a
//     dependency to a spec never moves an existing sandbox's addresses. Somebody's
//     DATABASE_PORT changing because a colleague added `depends_on` would be a much worse
//     bug than the one this fixes.
//   - It is not a wake order. The daemon wakes what is connected to, and after a sleep there
//     is no "startup" for an ordering rule to attach to. A service that needs another at
//     runtime retries - which it must anyway, because that is what waking looks like from
//     the inside.

import (
	"fmt"
	"sort"
	"strings"
)

// CreationOrder returns service names in an order that respects depends_on.
//
// Alphabetical within each dependency level, so a spec with no dependencies produces exactly
// what Names() always did and nothing about existing behaviour changes.
func (s *Spec) CreationOrder() ([]string, error) {
	if err := s.checkDependencies(); err != nil {
		return nil, err
	}

	var (
		out      []string
		done     = map[string]bool{}
		visiting = map[string]bool{}
	)

	// Depth-first, entered in alphabetical order, so the result is deterministic - the same
	// spec must always create in the same order or a failure is not reproducible.
	var visit func(string) error

	visit = func(name string) error {
		if done[name] {
			return nil
		}

		if visiting[name] {
			return fmt.Errorf("services depend on each other in a cycle, at %q - "+
				"nothing can be created first", name)
		}

		visiting[name] = true

		deps := append([]string(nil), s.Services[name].DependsOn...)
		sort.Strings(deps)

		for _, d := range deps {
			if err := visit(d); err != nil {
				return fmt.Errorf("%s → %w", name, err)
			}
		}

		visiting[name] = false
		done[name] = true

		out = append(out, name)

		return nil
	}

	for _, name := range s.Names() {
		if err := visit(name); err != nil {
			return nil, err
		}
	}

	return out, nil
}

// checkDependencies rejects a dependency on something that is not there.
//
// Caught at load rather than at create: a typo in a service name would otherwise be a
// dependency that silently never applied, which looks exactly like the race it was added to
// prevent and is far harder to find.
func (s *Spec) checkDependencies() error {
	for _, name := range s.Names() {
		for _, dep := range s.Services[name].DependsOn {
			if dep == name {
				return fmt.Errorf("service %q depends on itself", name)
			}

			if _, ok := s.Services[dep]; !ok {
				return fmt.Errorf("service %q depends on %q, which this spec does not declare "+
					"(it has: %s)", name, dep, strings.Join(s.Names(), ", "))
			}
		}
	}

	return nil
}
