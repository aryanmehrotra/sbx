package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/spec"
)

// cap_add exists for the workloads that cannot run without one and whose failure says nothing
// useful: CRIU reports "needs CAP_SYS_ADMIN or CAP_CHECKPOINT_RESTORE", a debugger fails on
// ptrace with a bare permission error. Neither is fixable from inside the container.

func TestKubernetesRefusesCapAddRatherThanEmittingIt(t *testing.T) {
	k := &kubeProvider{namespace: "sbx"}

	err := k.Create(context.Background(), "sb", 0, 0, "svc", spec.Service{
		Image:  "alpine:3.20",
		Ports:  []int{80},
		CapAdd: []string{"CHECKPOINT_RESTORE"},
	}, nil, "", IsolationContainer)
	if err == nil {
		t.Fatal("the cluster accepted cap_add; whether a pod may hold a capability is decided " +
			"by admission, so this would be rejected later with an error naming a policy " +
			"rather than the spec - or granted on a shared cluster")
	}

	if !strings.Contains(err.Error(), "cap_add") {
		t.Fatalf("the refusal does not name the field: %v", err)
	}
}

func TestCapAddSurvivesASpecRoundTrip(t *testing.T) {
	in := []byte(`{"version":1,"services":{"a":{"image":"alpine","ports":[80],
	   "cap_add":["CHECKPOINT_RESTORE","SYS_PTRACE"]}}}`)

	s, err := spec.ParseSpec(in, "")
	if err != nil {
		t.Fatal(err)
	}

	got := s.Services["a"].CapAdd
	if len(got) != 2 || got[0] != "CHECKPOINT_RESTORE" || got[1] != "SYS_PTRACE" {
		t.Fatalf("cap_add = %v, want both entries in order", got)
	}
}
