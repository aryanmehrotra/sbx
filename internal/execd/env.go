package execd

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/aryanmehrotra/sbx/internal/execdctl"
)

// hiddenEnv are execd's own settings, kept out of every process it starts. The access token
// above all: user code needs nothing from it, and a token readable by `env` inside the sandbox
// is one that ends up in a log.
var hiddenEnv = map[string]bool{
	EnvAccessToken:   true,
	EnvGraceShutdown: true,

	// The control secret is the host's, for seal and re-key; user code has no use for it.
	execdctl.EnvControlSecret: true,
}

// userEnv is the environment a command or session sees, as upstream layers it: execd's own
// environment, then the EXECD_ENVS file, then the request's envs.
func userEnv(request map[string]string) map[string]string {
	env := map[string]string{}

	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" || hiddenEnv[k] {
			continue
		}

		env[k] = v
	}

	for k, v := range extraEnvFile() {
		env[k] = v
	}

	for k, v := range request {
		env[k] = v
	}

	return env
}

// envList renders a map as exec.Cmd.Env, sorted so a child's environment does not change order
// from one run to the next.
func envList(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}

	sort.Strings(out)

	return out
}

// extraEnvFile reads the file named by EXECD_ENVS on every call rather than once, so an
// operator who edits it does not have to restart the sandbox's PID 1 to be heard. A file that
// cannot be read is logged-and-ignored upstream; here it is simply ignored, because the only
// place to log to is the container's stderr, which nobody is reading when a command runs.
func extraEnvFile() map[string]string {
	path := os.Getenv(EnvExtraEnvs)
	if path == "" {
		return nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	return parseEnvFile(string(data))
}

// parseEnvFile reads KEY=VALUE lines: blank lines and #-comments skipped, an optional leading
// "export ", and a value optionally in single quotes (literal) or double quotes (with \" \\ \n
// escapes). A malformed line is skipped rather than failing the file.
func parseEnvFile(data string) map[string]string {
	out := map[string]string{}

	for _, raw := range strings.Split(data, "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		line = strings.TrimPrefix(line, "export ")

		k, v, ok := strings.Cut(line, "=")
		k = strings.TrimSpace(k)

		if !ok || !validEnvKey(k) {
			continue
		}

		v = strings.TrimSpace(v)

		switch {
		case len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'':
			v = v[1 : len(v)-1]
		case len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"':
			v = strings.NewReplacer(`\"`, `"`, `\\`, `\`, `\n`, "\n").Replace(v[1 : len(v)-1])
		}

		out[k] = v
	}

	return out
}

func validEnvKey(k string) bool {
	if k == "" {
		return false
	}

	for i, r := range k {
		switch {
		case r == '_', r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}

	return true
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

// expandPath expands $NAME, ${NAME} and a leading ~ the way the spec describes cwd: from the
// given environment, failing on a variable it does not define. A silently empty variable would
// turn "$WORKDIR/src" into "/src", and a command would run somewhere nobody asked for.
func expandPath(path string, env map[string]string) (string, error) {
	if path == "" {
		return "", nil
	}

	var missing []string

	for _, m := range envRef.FindAllStringSubmatch(path, -1) {
		name := m[1] + m[2]
		if _, ok := env[name]; !ok {
			missing = append(missing, name)
		}
	}

	if len(missing) > 0 {
		sort.Strings(missing)
		return "", fmt.Errorf("path %q references undefined environment variables: %s; "+
			"pass them in envs or drop the reference", path, strings.Join(missing, ","))
	}

	out := os.Expand(path, func(k string) string { return env[k] })

	if out == "~" || strings.HasPrefix(out, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("expand ~ in %q: %w", path, err)
		}

		out = filepath.Join(home, out[1:])
	}

	return out, nil
}

// absPath is expandPath against execd's own environment, made absolute - the resolution
// upstream's file endpoints use (pathutil.ExpandAbsPath).
func absPath(path string) (string, error) {
	p, err := expandPath(path, userEnv(nil))
	if err != nil {
		return "", err
	}

	return filepath.Abs(p)
}
