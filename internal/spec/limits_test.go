package spec

import (
	"strings"
	"testing"
)

func limitSpec(field, val string) []byte {
	return []byte(`{"version":1,"services":{"a":{"image":"alpine","ports":[80],"` + field + `":"` + val + `"}}}`)
}

// Each of these passed `sbx validate` and failed only at create, after the image pull: docker
// 29.5.2 says `invalid size: 'lots'`, `range of CPUs is from 0.01 to ...` and `invalid count
// (lots): value must be either "all" or an integer`. No provider accepts any of them.
func TestLimitsNoProviderAcceptsAreRefusedAtLoad(t *testing.T) {
	for _, c := range []struct{ field, val string }{
		{"memory", "lots"}, {"memory", "-1g"}, {"memory", "512 x"}, {"memory", "g"},
		{"cpu", "-1"}, {"cpu", "-500m"}, {"cpu", "lots"}, {"cpu", "2 cores"}, {"cpu", "NaN"}, {"cpu", "Inf"},
		{"gpus", "lots"}, {"gpus", "gpu=0"}, {"gpus", "device=0,lots"}, {"gpus", "driver=nvidia,x=y"},
	} {
		_, err := ParseSpec(limitSpec(c.field, c.val), "spec.json")
		if err == nil {
			t.Errorf("%s %q was accepted", c.field, c.val)
			continue
		}

		for _, want := range []string{"spec.json", `service "a"`, c.field, `"` + c.val + `"`} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error for %s %q = %q, does not mention %s", c.field, c.val, err, want)
			}
		}
	}
}

// The spec is shared by three providers with different spellings - docker's "512m" and
// Kubernetes' "512Mi" and "500m" cores - so anything one of them takes must still load.
func TestLimitsEveryProvidersSpellingStillLoads(t *testing.T) {
	for _, c := range []struct{ field, val string }{
		{"memory", "512m"}, {"memory", "2g"}, {"memory", "512mb"}, {"memory", "1.5g"}, {"memory", "1024k"},
		{"memory", "536870912"}, {"memory", "512Mi"}, {"memory", "1Gi"}, {"memory", "1G"},
		{"memory", "1e9"}, {"memory", "0"}, {"cpu", "0"},
		{"cpu", "0.5"}, {"cpu", "2"}, {"cpu", "500m"},
		{"gpus", "all"}, {"gpus", "1"}, {"gpus", "device=0"}, {"gpus", "count=2,capabilities=gpu"},
		{"gpus", `\"device=0,1\"`}, {"gpus", "driver=nvidia,count=all"},
		{"gpus", "device=0,1"},
	} {
		if _, err := ParseSpec(limitSpec(c.field, c.val), "spec.json"); err != nil {
			t.Errorf("%s %q was refused: %v", c.field, c.val, err)
		}
	}
}
