package spec

import (
	"encoding/json"
	"strings"
	"testing"
)

// readonly_volumes and volume_mounts mount any named docker volume - another sandbox's data,
// the API's own sbx-osb-pvc-* claims - with none of the namespacing the OpenSandbox API applies
// before it sets them. They are for the API to set in memory, so a sandbox.json naming them is
// refused as an unknown field rather than honoured.
func TestInternalMountFieldsAreNotSpecFields(t *testing.T) {
	cases := map[string]string{
		"readonly_volumes": `{"version": 1, "services": {"web": {"image": "nginx", "ports": [80],
			"readonly_volumes": {"sbx-osb-pvc-victim": "/steal"}}}}`,
		"volume_mounts": `{"version": 1, "services": {"web": {"image": "nginx", "ports": [80],
			"volume_mounts": [{"volume": "sbx-other-data", "target": "/steal"}]}}}`,
	}

	for field, raw := range cases {
		_, err := ParseSpec([]byte(raw), "sandbox.json")
		if err == nil || !strings.Contains(err.Error(), "unknown field") ||
			!strings.Contains(err.Error(), field) {
			t.Errorf("%s: want an unknown-field refusal naming it, got %v", field, err)
		}
	}
}

// Nor does a Service carrying them serialise them: nothing that writes a spec out can turn an
// API sandbox's mounts into a sandbox.json that grants them.
func TestInternalMountFieldsDoNotSerialise(t *testing.T) {
	svc := Service{Image: "x", Ports: []int{1},
		ReadOnlyVolumes: map[string]string{"sbx-execd-dev": "/opt/sbx"},
		VolumeMounts:    []VolumeMount{{Volume: "sbx-osb-pvc-data", Target: "/data"}}}

	body, err := json.Marshal(svc)
	if err != nil {
		t.Fatal(err)
	}

	for _, key := range []string{"readonly_volumes", "volume_mounts", "sbx-osb-pvc-data"} {
		if strings.Contains(string(body), key) {
			t.Errorf("%s serialised: %s", key, body)
		}
	}
}
