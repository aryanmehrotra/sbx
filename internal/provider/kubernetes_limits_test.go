package provider

import (
	"errors"
	"strings"
	"testing"
)

// A refusal to patch is the one failure here somebody will actually hit, and the apiserver's
// own words do not explain it: setting a ceiling patches the Deployment, and the Role this
// project ships grants `deployments/scale` and nothing else. Measured against a live cluster -
// with that Role bound, `kubectl auth can-i patch deployments` answers no.
func TestAForbiddenPatchExplainsWhichPermissionIsMissing(t *testing.T) {
	// The apiserver's real wording, from a minikube run with the activator Role bound.
	real := errors.New(`Error from server (Forbidden): deployments.apps "probe" is forbidden: ` +
		`User "system:serviceaccount:sbx:sbx-activator" cannot patch resource "deployments" ` +
		`in API group "apps" in the namespace "sbx"`)

	got := limitsPatchError("sbx-a-b", real).Error()

	for _, want := range []string{"may not set limits", "patch", "deployments/scale", "kubeconfig"} {
		if !strings.Contains(got, want) {
			t.Errorf("a forbidden patch was reported as %q, which never mentions %q", got, want)
		}
	}

	// The apiserver's own text is kept: it names the user and namespace, which is what the
	// reader needs to fix it.
	if !strings.Contains(got, "sbx-activator") {
		t.Error("the explanation dropped the apiserver's message, so the reader cannot see which identity was refused")
	}
}

// Everything else is passed through unchanged. A network error dressed up as an RBAC lecture
// would send the reader to the wrong place entirely.
func TestOtherFailuresAreNotBlamedOnRBAC(t *testing.T) {
	got := limitsPatchError("sbx-a-b", errors.New("dial tcp 10.0.0.1:6443: connect: connection refused")).Error()

	if strings.Contains(got, "deployments/scale") {
		t.Errorf("an unreachable apiserver was reported as a permissions problem: %q", got)
	}

	if !strings.Contains(got, "connection refused") {
		t.Errorf("the original failure was lost: %q", got)
	}
}
