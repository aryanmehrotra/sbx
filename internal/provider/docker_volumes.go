package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

// labelHostBinds pins the host directories a container binds: the canonical paths the
// OpenSandbox API validated, as a JSON array.
const labelHostBinds = "sbx.host-binds"

// hostBindsLabel is the pin for a service's host volumes, or "" when it has none. Only
// VolumeMounts carry a host path here - they are the API's, never a sandbox.json field - and each
// was resolved to its canonical form before create, which is what makes "still resolves to
// exactly itself" the right test at every later start.
func hostBindsLabel(mounts []spec.VolumeMount) string {
	var hosts []string

	for _, m := range mounts {
		if m.Host != "" {
			hosts = append(hosts, m.Host)
		}
	}

	if len(hosts) == 0 {
		return ""
	}

	b, _ := json.Marshal(hosts)

	return string(b)
}

// HostBindChanged is a start refused because a pinned host volume no longer resolves to the
// directory create validated.
type HostBindChanged struct {
	Ref, Path, Now string
	Err            error
}

func (e *HostBindChanged) Error() string {
	switch {
	case e.Err != nil:
		return fmt.Sprintf("%s was not started: its host volume %s is not the directory it was "+
			"created with (%v). sbx starts a sandbox only on the directory the API validated - put "+
			"it back, or remove the sandbox", e.Ref, e.Path, e.Err)
	default:
		return fmt.Sprintf("%s was not started: its host volume %s now leads to %s, and docker "+
			"would follow it. sbx starts a sandbox only on the directory the API validated - put it "+
			"back, or remove the sandbox", e.Ref, e.Path, e.Now)
	}
}

func (e *HostBindChanged) Unwrap() error { return e.Err }

// checkHostBinds refuses a start whose pinned host volumes changed since create.
//
// Docker resolves a bind source again on every container start, following any symlink it
// meets. The API validated each host path when the sandbox was created - under an allowed
// root, resolving to itself - and nothing held it there afterwards: a directory swapped for a
// link while the sandbox slept was followed at the next wake, to anything the engine can read.
// So every start re-asks the create-time question first and fails closed. The window left is
// the one create also has: between this check and docker's own resolution, milliseconds.
//
// A container with no pin (every sandbox.json container) costs nothing but the inspect, and a
// container that cannot be inspected is left to the start to report.
func (d *dockerProvider) checkHostBinds(ctx context.Context, ref string) error {
	c, ok, err := d.api.inspect(ctx, ref)
	if err != nil || !ok {
		return nil
	}

	raw := c.Labels[labelHostBinds]
	if raw == "" {
		return nil
	}

	var hosts []string
	if err := json.Unmarshal([]byte(raw), &hosts); err != nil {
		return &HostBindChanged{Ref: ref, Path: raw, Err: fmt.Errorf("its pin is unreadable: %w", err)}
	}

	for _, h := range hosts {
		if err := unchangedDir(h); err != nil {
			return withRef(err, ref)
		}
	}

	return nil
}

// unchangedDir reports whether p still resolves to exactly itself and is a directory.
func unchangedDir(p string) error {
	now, err := filepath.EvalSymlinks(p)
	if err != nil {
		return &HostBindChanged{Path: p, Err: err}
	}

	if now != p {
		return &HostBindChanged{Path: p, Now: now}
	}

	if fi, err := os.Stat(now); err != nil {
		return &HostBindChanged{Path: p, Err: err}
	} else if !fi.IsDir() {
		return &HostBindChanged{Path: p, Err: errors.New("it is no longer a directory")}
	}

	return nil
}

func withRef(err error, ref string) error {
	var hb *HostBindChanged
	if errors.As(err, &hb) {
		hb.Ref = ref
	}

	return err
}
