// Package execd is the daemon that runs inside a sandbox created through the OpenSandbox API.
// It speaks OpenSandbox's execd protocol (specs/execd-api.yaml at release-1.1.0): commands with
// SSE output, background commands whose output can be polled, bash sessions, files,
// directories and metrics.
//
// It is written from the spec, with upstream's execd (components/execd at the same tag) as the
// reference for behaviour wherever the spec is silent. Upstream is built on gin and a dozen
// other modules; this is the standard library only, because it ships inside the one `sbx`
// binary and the root module has no dependencies.
//
// It is run as `sbx execd [--addr :44772] [-- cmd args...]`, usually as a container's PID 1.
// Given a command, it starts that as its child, forwards signals to it, reaps whatever gets
// re-parented to it, and exits with the child's status - so the image's own entrypoint behaves
// as it would have without execd in front of it.
package execd

// The environment contract with whoever starts execd. The names are upstream's, so a sandbox
// provisioned by OpenSandbox's own server would configure this daemon the same way.
const (
	// EnvAccessToken holds the token every request except /ping must present in the
	// X-EXECD-ACCESS-TOKEN header. Empty or unset means no authentication, as upstream.
	EnvAccessToken = "EXECD_ACCESS_TOKEN"

	// EnvGraceShutdown is a Go duration ("200ms", "2s"): how long execd keeps serving after its
	// child exits, so a client can still read the last command's output. Upstream default.
	EnvGraceShutdown = "EXECD_API_GRACE_SHUTDOWN"

	// EnvExtraEnvs names a file of KEY=VALUE lines layered over execd's own environment for
	// every command and session it starts.
	EnvExtraEnvs = "EXECD_ENVS"

	// AccessTokenHeader is the request header the token travels in.
	AccessTokenHeader = "X-EXECD-ACCESS-TOKEN"

	// DefaultAddr is the port OpenSandbox clients expect execd on.
	DefaultAddr = ":44772"
)
