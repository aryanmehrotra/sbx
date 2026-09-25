package spec

import (
	"strings"
	"testing"
)

func TestVolumeMountsMustBeWellFormed(t *testing.T) {
	ok := []VolumeMount{
		{Volume: "sbx-osb-pvc-data", Target: "/data"},
		{Volume: "sbx-osb-pvc-data", Target: "/data", SubPath: "a/b", ReadOnly: true},
		{Host: "/Users/me/work", Target: "/work"},
	}

	for _, m := range ok {
		s := Service{Image: "x", Ports: []int{1}, VolumeMounts: []VolumeMount{m}}
		if err := s.validate("svc"); err != nil {
			t.Errorf("%+v refused: %v", m, err)
		}
	}

	bad := map[string]VolumeMount{
		"exactly one":       {Target: "/data"},
		"exactly one ":      {Volume: "v", Host: "/h", Target: "/data"},
		"not a volume name": {Volume: "a/b", Target: "/data"},
		"absolute":          {Host: "rel", Target: "/data"},
		"sub_path is":       {Host: "/h", Target: "/data", SubPath: "x"},
		"target":            {Volume: "v", Target: "data"},
		"must not":          {Volume: "v", Target: "/data", SubPath: "../etc"},
		"must not ":         {Volume: "v", Target: "/data", SubPath: "/etc"},
		"comma":             {Host: "/a,readonly=false", Target: "/data"},
		"comma, quote":      {Volume: "v", Target: "/data\"x"},
		"must not  ":        {Volume: "v", Target: "/data", SubPath: "a/../../b"},
		"a comma, quot":     {Volume: "v", Target: "/d", SubPath: "a,b"},
	}

	for want, m := range bad {
		s := Service{Image: "x", Ports: []int{1}, VolumeMounts: []VolumeMount{m}}

		err := s.validate("svc")
		if err == nil || !strings.Contains(err.Error(), strings.TrimSpace(want)) {
			t.Errorf("%+v: err %v, want one saying %q", m, err, want)
		}
	}
}
