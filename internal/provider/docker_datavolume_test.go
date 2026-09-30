package provider

import "testing"

// cli warns about a leftover data volume only through this method; if docker stopped providing
// it, the warning would go silent without anything failing.
func TestDockerNamesTheDataVolumeItMounts(t *testing.T) {
	var p any = &dockerProvider{}

	dv, ok := p.(interface {
		DataVolume(sandbox, service string) string
	})
	if !ok {
		t.Fatal("docker no longer names its data volumes, so create cannot warn about a leftover one")
	}

	if got := dv.DataVolume("qa", "redis"); got != "sbx-qa-redis-data" {
		t.Errorf("DataVolume = %q, want the name `docker run -v` mounts: sbx-qa-redis-data", got)
	}
}
