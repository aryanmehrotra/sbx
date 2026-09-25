package osb

import (
	"net/http"
	"strings"
	"testing"
)

// OPENSANDBOX_EGRESS_* configure the egress sidecar, not the workload (upstream
// server/opensandbox_server/services/helpers.py split_egress_env at release-1.1.0): they must
// never reach the sandbox container, an unknown one is a 400, and without a networkPolicy
// there is no sidecar, so they are dropped.
func TestEgressEnvIsSplitFromTheSandboxEnv(t *testing.T) {
	h := newHarness(t)

	for name, np := range map[string]any{"with policy": map[string]any{"defaultAction": "allow"}, "without": nil} {
		b := minimalCreate()
		b["env"] = map[string]string{
			"OPENSANDBOX_EGRESS_LOG_LEVEL":             "debug",
			"OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT": "true",
			"MY_APP_VAR": "hello",
		}

		if np != nil {
			b["networkPolicy"] = np
		}

		env := h.p.service(h.create(b).ID).Env

		if _, leaked := env["OPENSANDBOX_EGRESS_LOG_LEVEL"]; leaked {
			t.Errorf("%s: OPENSANDBOX_EGRESS_LOG_LEVEL reached the sandbox container", name)
		}

		if env["MY_APP_VAR"] != "hello" {
			t.Errorf("%s: a regular variable was lost: %v", name, env)
		}

		// Upstream deliberately gives the workload this one too: it has to know its traffic is
		// being intercepted.
		if env["OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT"] != "true" {
			t.Errorf("%s: MITMPROXY_TRANSPARENT is the one egress variable the sandbox also gets", name)
		}
	}
}

func TestReservedEgressEnvIsRefused(t *testing.T) {
	h := newHarness(t)

	b := minimalCreate()
	b["env"] = map[string]string{"OPENSANDBOX_EGRESS_RULES": "x"}
	b["networkPolicy"] = map[string]any{"defaultAction": "allow"}

	resp := h.do("POST", "/v1/sandboxes", b, nil)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(h.errOf(resp).Message, "OPENSANDBOX_EGRESS_RULES") {
		t.Fatalf("a reserved egress variable = %d, want 400 naming it", resp.StatusCode)
	}
}

// Checked before credentialProxy is refused as unsupported: the combination is invalid whether
// or not this server implements the proxy, and the caller should hear the precise reason.
func TestSSLInsecureWithCredentialProxyIs400(t *testing.T) {
	h := newHarness(t)

	b := minimalCreate()
	b["env"] = map[string]string{"OPENSANDBOX_EGRESS_MITMPROXY_SSL_INSECURE": "true"}
	b["networkPolicy"] = map[string]any{"defaultAction": "allow"}
	b["credentialProxy"] = map[string]any{"enabled": true}

	resp := h.do("POST", "/v1/sandboxes", b, nil)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(h.errOf(resp).Message, "SSL_INSECURE") {
		t.Fatalf("SSL_INSECURE with credentialProxy = %d, want 400", resp.StatusCode)
	}
}
