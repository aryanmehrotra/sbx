package osb

// OPENSANDBOX_EGRESS_* in a create request's env configure the egress sidecar, not the
// workload. Upstream splits them off at create (server/opensandbox_server/services/helpers.py
// split_egress_env and services/constants.py ALLOWED_EGRESS_ENV_VARS, release-1.1.0), and a
// client relies on three things about that: none of them reaches the sandbox container, one not
// on the allow-list is a 400, and without a networkPolicy - so no sidecar - they are dropped
// rather than refused.
//
// sbx's filter is not upstream's sidecar and reads none of these tunables yet, so an accepted
// one is recorded (in the sandbox's history) rather than applied. That is the honest middle:
// refusing a variable upstream accepts would break clients that set it for their sidecar, and
// passing it into the workload is the leak upstream's own tests check for.

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

const (
	egressEnvPrefix   = "OPENSANDBOX_EGRESS_"
	egressSSLInsecure = "OPENSANDBOX_EGRESS_MITMPROXY_SSL_INSECURE"
	egressTransparent = "OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT"
)

// allowedEgressEnv is upstream's ALLOWED_EGRESS_ENV_VARS, verbatim.
var allowedEgressEnv = []string{
	"OPENSANDBOX_EGRESS_LOG_LEVEL",
	"OPENSANDBOX_EGRESS_DNS_UPSTREAM_TIMEOUT",
	egressSSLInsecure,
	egressTransparent,
	"OPENSANDBOX_EGRESS_MITMPROXY_EXTRA_PORTS",
	"OPENSANDBOX_EGRESS_DENY_WEBHOOK",
	"OPENSANDBOX_EGRESS_METRICS_EXTRA_ATTRS",
	"OPENSANDBOX_EGRESS_CREDENTIAL_VAULT_REQUIRE_TLS",
	"OPENSANDBOX_EGRESS_CREDENTIAL_VAULT_REQUIRE_SCOPED_MATCH",
	"OPENSANDBOX_EGRESS_CREDENTIAL_VAULT_TRUSTED_PROXY_CIDRS",
	"OPENSANDBOX_EGRESS_POLICY_FILE",
}

// splitEgressEnv returns the workload's env and the egress sidecar's, or the refusal for a
// reserved name. MITMPROXY_TRANSPARENT goes to both, as upstream sends it: the workload has to
// know its traffic is being intercepted.
func splitEgressEnv(env map[string]string) (sandbox, egressEnv map[string]string, err error) {
	sandbox, egressEnv = map[string]string{}, map[string]string{}

	for k, v := range env {
		if !strings.HasPrefix(k, egressEnvPrefix) {
			sandbox[k] = v
			continue
		}

		if !slices.Contains(allowedEgressEnv, k) {
			return nil, nil, fmt.Errorf("environment variable %q is not allowed: OPENSANDBOX_EGRESS_* "+
				"configure the egress sidecar, and the permitted ones are %s",
				k, strings.Join(allowedEgressEnv, ", "))
		}

		egressEnv[k] = v

		if k == egressTransparent {
			sandbox[k] = v
		}
	}

	return sandbox, egressEnv, nil
}

// credentialProxyEnabled reports whether the create asked for a credential proxy. A field that
// does not say is off, which is what the schema's default says it is; a field that cannot be read
// is a refusal, from credentialProxyRequest.
func credentialProxyEnabled(raw json.RawMessage) bool {
	on, _ := credentialProxyRequest(raw)

	return on
}

// credentialProxyRequest reads the create request's credentialProxy, or says why it cannot.
//
//	absent, null, {}, {"enabled": false}   off
//	{"enabled": true}                      on
//	anything else                          refused
//
// The last line is the point. CredentialProxyConfig is `additionalProperties: false` with
// `enabled` defaulting to false (specs/sandbox-lifecycle.yml at the pinned commit), so a
// misspelled field or a quoted boolean is a request the reference server rejects - and reading it
// as "off" instead would hand somebody who wrote {"enabled": "true"} a sandbox with no credential
// protection and a 202 saying it worked. A security feature may be off, but not by accident.
//
// An empty object is NOT refused, however much it looks like one: the schema gives `enabled` a
// default of false, so {} is a legal way to say off and upstream's own server reads it that way
// (server/opensandbox_server/services/validators.py at the pinned commit). Refusing it would
// refuse a request that works against the reference implementation.
func credentialProxyRequest(raw json.RawMessage) (enabled bool, err error) {
	t := strings.TrimSpace(string(raw))
	if t == "" || t == "null" {
		return false, nil
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return false, fmt.Errorf("credentialProxy is an object with one field, enabled: "+
			"{\"credentialProxy\": {\"enabled\": true}}, not %s", t)
	}

	for k := range fields {
		if k != "enabled" {
			return false, fmt.Errorf("credentialProxy has no %q field - its only field is "+
				"enabled, and a sandbox created with a field this server ignores would have no "+
				"credential protection and no error saying so. Send "+
				"{\"credentialProxy\": {\"enabled\": true}}", k)
		}
	}

	got, ok := fields["enabled"]
	if !ok {
		return false, nil
	}

	if err := json.Unmarshal(got, &enabled); err != nil {
		return false, fmt.Errorf("credentialProxy.enabled is %s, which is not true or false: a "+
			"quoted boolean would be read as off, so it is refused instead", got)
	}

	return enabled, nil
}
