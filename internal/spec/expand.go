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
//   - `$${` is a literal `${`, compose's spelling. Without an escape a value that genuinely
//     contains `${HOME}` - a password, a template string for the program inside - could not be
//     written at all once every other `${` form became an error.
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

// A reference that starts exactly where a `${` does. Anchored, so each `${` in a value is judged
// on its own rather than by whether some other part of the value happens to match.
var envRefAt = regexp.MustCompile(`^\$\{[A-Za-z_][A-Za-z0-9_]*\}`)

// scanEnv is the one reading of an env value, shared by the syntax check and the expansion so
// the two cannot disagree about what is a reference. Left to right: `$${` is emitted as `${`
// and never looked at again, ${NAME} is replaced by sub(NAME, ref), and any other `${` is
// passed to bad (with the text up to its closing brace) and kept as written. Every other byte,
// including a bare `$` or `$$`, is kept: only `${` claims this syntax.
func scanEnv(val string, sub func(name, ref string) string, bad func(ref string)) string {
	if !strings.Contains(val, "${") {
		return val
	}

	var b strings.Builder

	for i := 0; i < len(val); {
		rest := val[i:]

		switch {
		case strings.HasPrefix(rest, "$${"):
			b.WriteString("${")
			i += 3
		case strings.HasPrefix(rest, "${"):
			if m := envRefAt.FindString(rest); m != "" {
				b.WriteString(sub(m[2:len(m)-1], m))
				i += len(m)

				continue
			}

			// Quote the reference, not the whole value: the text around it may be a secret.
			ref := rest
			if end := strings.IndexByte(ref, '}'); end >= 0 {
				ref = ref[:end+1]
			}

			bad(ref)
			b.WriteString(ref)
			i += len(ref)
		default:
			b.WriteByte(val[i])
			i++
		}
	}

	return b.String()
}

// expandEnv resolves ${VAR} in every service's env values, or says which are missing. It must
// run once per load: its output can contain a literal `${` from a `$${`, which a second pass
// would read as a reference.
func (s *Spec) expandEnv(lookup func(string) (string, bool)) error {
	return s.resolveEnv(lookup, true)
}

// unsetEnv is expandEnv without the writes: which referenced variables are not set, for a load
// that is already failing on syntax and must not stop at that (checkEnvSyntax's caller).
func (s *Spec) unsetEnv(lookup func(string) (string, bool)) error {
	return s.resolveEnv(lookup, false)
}

func (s *Spec) resolveEnv(lookup func(string) (string, bool), write bool) error {
	missing := map[string][]string{}

	for _, name := range s.Names() {
		svc := s.Services[name]

		for key, val := range svc.Env {
			out := scanEnv(val, func(varName, ref string) string {
				got, ok := lookup(varName)
				if !ok {
					missing[varName] = append(missing[varName], name+"."+key)

					return ref
				}

				return got
			}, func(string) {})

			if write {
				svc.Env[key] = out
			}
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

// checkEnvSyntax refuses every `${` in the spec's env values that is neither the plain ${NAME}
// form nor the `$${` escape, all of them in one error.
//
// Without it `${X:-y}` matched neither envRef nor any refusal and reached the container as
// the literal string "${X:-y}" - the looks-like-success failure the comment at the top of this
// file exists to prevent. It is syntax, so it is checked at load whether or not expansion runs.
// All at once for the same reason unset variables are: one per run is one failed validate per
// mistake.
func (s *Spec) checkEnvSyntax() error {
	var found []string

	for _, name := range s.Names() {
		env := s.Services[name].Env

		keys := make([]string, 0, len(env))
		for k := range env {
			keys = append(keys, k)
		}

		sort.Strings(keys)

		for _, key := range keys {
			var refs []string

			scanEnv(env[key], func(_, ref string) string { return ref },
				func(ref string) { refs = append(refs, fmt.Sprintf("%q", ref)) })

			if len(refs) > 0 {
				found = append(found, fmt.Sprintf("%s.%s uses %s", name, key, strings.Join(refs, ", ")))
			}
		}
	}

	if len(found) == 0 {
		return nil
	}

	return fmt.Errorf("env values use ${...} forms sbx does not expand: %s - only the plain "+
		"${NAME} form works, with no defaults or nesting; compute the value in your shell and "+
		"reference it as ${NAME}, or write $${ for a literal ${", strings.Join(found, "; "))
}
