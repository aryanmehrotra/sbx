package app

import (
	"slices"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/egress"
	"github.com/aryanmehrotra/sbx/internal/spec"
)

// `sbx prewarm --spec F` fetches the volume-copy helper exactly when a snapshot or fork of F
// would run it: a service declares `volume`.
func TestPrewarmHelpersFollowTheSpecsVolumes(t *testing.T) {
	with := &spec.Spec{Services: map[string]spec.Service{
		"web": {Image: "nginx"}, "db": {Image: "postgres:16", Volume: "/var/lib/postgresql/data"},
	}}
	without := &spec.Spec{Services: map[string]spec.Service{"web": {Image: "nginx"}}}

	if !copiesVolumes(with) {
		t.Error("a spec with a volume does not get the helper")
	}

	if copiesVolumes(without) {
		t.Error("a spec with no volume gets a helper it never runs")
	}
}

// Two services on one image are one pull: `sbx prewarm --spec` listed redis:7-alpine twice and
// counted it twice in "N already present".
func TestSpecImagesNamesEachImageOnce(t *testing.T) {
	s := &spec.Spec{Services: map[string]spec.Service{
		"db": {Image: "redis:7-alpine"}, "kv": {Image: "redis:7-alpine"}, "web": {Image: "nginx"}, "b": {},
	}}

	if got := specImages(s); !slices.Equal(got, []string{"nginx", "redis:7-alpine"}) {
		t.Errorf("specImages = %v, want each image once", got)
	}
}

// A filtered service makes the daemon build the egress filter, from two pinned images a spec
// never names; prewarm fetches them exactly when some service is filtered.
func TestPrewarmHelpersFollowTheSpecsEgress(t *testing.T) {
	for name, svc := range map[string]spec.Service{
		"egress_allow":  {Image: "app", EgressAllow: []string{"api.example.com"}},
		"egress allow":  {Image: "app", Egress: spec.EgressAllow},
		"egress_policy": {Image: "app", EgressPolicy: &egress.Policy{}},
	} {
		s := &spec.Spec{Services: map[string]spec.Service{"web": {Image: "nginx"}, "app": svc}}
		if !helperNeeds(s).Egress {
			t.Errorf("%s: a filtered spec does not get the filter's images", name)
		}
	}

	plain := &spec.Spec{Services: map[string]spec.Service{"web": {Image: "nginx", Egress: spec.EgressDeny}}}
	if helperNeeds(plain).Egress {
		t.Error("a spec with no filtered service gets the filter's images")
	}
}
