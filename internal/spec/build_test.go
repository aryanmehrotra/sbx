package spec

import "testing"

// image and build are alternatives. Neither is an error, and both is an error rather than
// a silent precedence rule - which of the two wins is exactly what a reader would guess
// wrong, and guessing means running a different image than the file appears to describe.
func TestImageOrBuild(t *testing.T) {
	cases := []struct {
		name string
		svc  Service
		ok   bool
	}{
		{"image only", Service{Image: "nginx", Ports: []int{80}}, true},
		{"build only", Service{Build: &Build{Context: "."}, Ports: []int{80}}, true},
		{"neither", Service{Ports: []int{80}}, false},
		{"both", Service{Image: "nginx", Build: &Build{Context: "."}, Ports: []int{80}}, false},
		{"build with no context", Service{Build: &Build{}, Ports: []int{80}}, false},
	}

	for _, c := range cases {
		err := c.svc.validate("svc")
		if c.ok && err != nil {
			t.Errorf("%s: unexpected error: %v", c.name, err)
		}

		if !c.ok && err == nil {
			t.Errorf("%s: accepted, but it is not a valid service", c.name)
		}
	}
}
