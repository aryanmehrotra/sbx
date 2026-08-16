package spec

// ${VAR} in a service's environment.
//
// sandbox.json is a file the docs tell people to commit, and until now the only place to put
// a value was inline. For `POSTGRES_PASSWORD: "app"` on a throwaway local database that is
// fine and will stay fine. For a private registry credential, or a real API key some fixture
// seeding needs, it means a secret in git.
//
// So a value may reference the environment sbx was invoked with. Deliberately the smallest
// possible version of this:
//
//   - Only `env` values. Not images, not health commands, not init steps. Expansion in a
//     command string is where this stops being substitution and starts being a shell.
//   - No defaults, no nesting, no `${VAR:-fallback}`. Each of those is a small syntax nobody
//     asked for and everybody has to learn, and the shell already has all of them for the
//     cases that need one.
//   - An unset variable is an error, not an empty string. A database that came up with an
//     empty password because a variable was not exported is the kind of failure that looks
//     like success, and this project has already been bitten by one of those.
//
// Anything beyond this - Vault, 1Password, a cloud secret manager - stays out. It would mean
// a dependency, a network call and a credential to fetch the credential, in a binary whose
// whole claim is that it has none of those.

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// ${NAME}, where NAME is the usual environment-variable shape. A bare $NAME is deliberately
// not matched: braces make the boundary unambiguous, and a password containing a literal `$`
// should not silently become a substitution.
var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnv resolves ${VAR} in every service's env values, or says which are missing.
func (s *Spec) expandEnv(lookup func(string) (string, bool)) error {
	missing := map[string][]string{}

	for _, name := range s.Names() {
		svc := s.Services[name]

		for key, val := range svc.Env {
			svc.Env[key] = envRef.ReplaceAllStringFunc(val, func(ref string) string {
				varName := ref[2 : len(ref)-1]

				got, ok := lookup(varName)
				if !ok {
					missing[varName] = append(missing[varName], name+"."+key)

					return ref
				}

				return got
			})
		}

		s.Services[name] = svc
	}

	if len(missing) == 0 {
		return nil
	}

	// Every missing variable at once. Reporting them one per run means one failed create per
	// variable, which for a spec with three of them is three round trips to find out what
	// the environment needed.
	names := make([]string, 0, len(missing))
	for v := range missing {
		names = append(names, v)
	}

	sort.Strings(names)

	parts := make([]string, 0, len(names))
	for _, v := range names {
		sort.Strings(missing[v])
		parts = append(parts, fmt.Sprintf("%s (used by %s)", v, strings.Join(missing[v], ", ")))
	}

	return fmt.Errorf("these environment variables are referenced but not set: %s",
		strings.Join(parts, "; "))
}

// osLookup is expandEnv's default source, separated so tests do not have to mutate the
// process environment to exercise the interesting cases.
func osLookup(name string) (string, bool) { return os.LookupEnv(name) }
