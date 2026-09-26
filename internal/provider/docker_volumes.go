package provider

import (
	"context"
	"sort"
	"strings"

	"github.com/aryanmehrotra/sbx/internal/spec"
)

// mountOption renders one volume_mounts entry as docker's --mount value. spec validation has
// already refused anything containing a comma or a quote, which is what makes plain joining
// safe here.
func mountOption(m spec.VolumeMount) string {
	var parts []string

	if m.Volume != "" {
		parts = append(parts, "type=volume", "src="+m.Volume, "dst="+m.Target)

		// volume-subpath needs API 1.45 (docker 26). An older engine refuses the whole
		// --mount with "unknown option", which is the honest outcome: the alternative - the
		// host-side Mountpoint as a bind - only works when the engine shares a filesystem
		// with the caller, which a Mac's VM does not.
		if m.SubPath != "" {
			parts = append(parts, "volume-subpath="+m.SubPath)
		}
	} else {
		parts = append(parts, "type=bind", "src="+m.Host, "dst="+m.Target)
	}

	if m.ReadOnly {
		parts = append(parts, "readonly")
	}

	return strings.Join(parts, ",")
}

// HostVolumes: docker binds a directory of this machine (--mount type=bind).
func (d *dockerProvider) HostVolumes() {}

var _ HostVolumes = (*dockerProvider)(nil)

// VolumeExists reports whether a named volume exists. An inspect that fails for any reason
// other than "no such volume" is an error, not an absence: reporting a volume missing because
// the engine was briefly unreachable would have the caller create a second, empty one.
func (d *dockerProvider) VolumeExists(_ context.Context, name string) (bool, error) {
	_, err := d.docker("volume", "inspect", name)
	if err == nil {
		return true, nil
	}

	if strings.Contains(strings.ToLower(err.Error()), "no such volume") {
		return false, nil
	}

	return false, err
}

func (d *dockerProvider) CreateVolume(_ context.Context, name string, labels map[string]string) error {
	args := []string{"volume", "create"}

	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	for _, k := range keys {
		args = append(args, "--label", k+"="+labels[k])
	}

	_, err := d.docker(append(args, name)...)

	return err
}

func (d *dockerProvider) RemoveVolume(_ context.Context, name string) error {
	_, err := d.docker("volume", "rm", name)
	return err
}

// RemoveImage deletes a saved image. An image that is already gone is success: the caller
// wanted it not to exist.
func (d *dockerProvider) RemoveImage(_ context.Context, image string) error {
	_, err := d.docker("rmi", image)
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "no such image") {
		return nil
	}

	return err
}
