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

// credentialProxyEnabled reads {"enabled": true} out of the raw credentialProxy field.
func credentialProxyEnabled(raw json.RawMessage) bool {
	var cp struct {
		Enabled bool `json:"enabled"`
	}

	return present(raw) && json.Unmarshal(raw, &cp) == nil && cp.Enabled
}
