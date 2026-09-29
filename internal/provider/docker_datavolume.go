package provider

// DataVolume names the volume a service's `volume` field mounts in sandbox. Create asks for it
// on a new sandbox to warn when that volume is already there: the name is derived from the
// sandbox and service, so a new sandbox silently took over one an earlier sandbox of the same
// name had left - an orphan from a failed fork - and started on its data.
func (d *dockerProvider) DataVolume(sandbox, service string) string {
	return volumeName(sandbox, service)
}
