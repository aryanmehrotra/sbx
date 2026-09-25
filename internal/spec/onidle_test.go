package spec

import (
	"strings"
	"testing"
)

// on_idle is a lifecycle switch, so a typo must fail rather than silently mean "stop": a
// sandbox whose author asked for "freze" and got its background process killed would have no
// way to find out why from anything sbx printed.
func TestOnIdleRefusesAnythingButStopOrFreeze(t *testing.T) {
	for _, v := range []string{"", "stop", "freeze"} {
		if err := (Service{Image: "x", Ports: []int{1}, OnIdle: v}).Validate("s"); err != nil {
			t.Errorf("on_idle %q refused: %v", v, err)
		}
	}

	err := (Service{Image: "x", Ports: []int{1}, OnIdle: "freze"}).Validate("s")
	if err == nil || !strings.Contains(err.Error(), "freeze") {
		t.Fatalf("on_idle typo accepted or unexplained: %v", err)
	}
}

// A readonly volume is a NAME. A host path here would be read by docker as a bind mount of a
// directory the VM may not share, which is the failure the field exists to avoid.
func TestReadOnlyVolumesAreNamesMountedAtAbsolutePaths(t *testing.T) {
	ok := Service{Image: "x", Ports: []int{1}, ReadOnlyVolumes: map[string]string{"sbx-execd-dev": "/opt/sbx"}}
	if err := ok.Validate("s"); err != nil {
		t.Fatalf("valid readonly volume refused: %v", err)
	}

	for vol, dest := range map[string]string{
		"/host/dir":  "/opt/sbx",
		"a:b":        "/opt/sbx",
		" ":          "/opt/sbx",
		"sbx-execd":  "opt/sbx",
		"sbx-execd2": "",
	} {
		s := Service{Image: "x", Ports: []int{1}, ReadOnlyVolumes: map[string]string{vol: dest}}
		if err := s.Validate("s"); err == nil {
			t.Errorf("readonly volume %q -> %q accepted", vol, dest)
		}
	}
}
