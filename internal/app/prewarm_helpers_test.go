package app

import (
	"testing"

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
