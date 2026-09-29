package provider

import (
	"slices"
	"testing"
)

// Docker's helpers are the images it actually runs: the volume copy's, and the egress filter's
// pinned builder and runtime (the ones ensureFilterImage builds from), each only when asked.
func TestDockerHelperImages(t *testing.T) {
	d := &dockerProvider{}

	images := func(n HelperNeeds) []string {
		var out []string
		for _, h := range d.HelperImages(n) {
			out = append(out, h.Image)
		}

		return out
	}

	if got := images(HelperNeeds{}); len(got) != 0 {
		t.Errorf("nothing asked for, got %v", got)
	}

	if got := images(HelperNeeds{Volumes: true}); !slices.Equal(got, []string{VolumeCopyImage}) {
		t.Errorf("volumes: %v", got)
	}

	if got := images(HelperNeeds{Egress: true}); !slices.Equal(got, []string{filterBuilderImage, filterRuntimeImage}) {
		t.Errorf("egress: %v, want the filter's builder and runtime", got)
	}
}
