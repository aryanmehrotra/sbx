package daemon

// Pinning: keeping a sandbox out of the reaper's reach for a while, without a label.
//
// keepAwake is a label, fixed when the container is made. The OpenSandbox warm pool needs the
// same thing for a stretch of a sandbox's life only: a member waits for its caller with no
// traffic at all, and the reaper would freeze it - so the claim would pay a thaw, which is a
// docker call that dockerd serialises, measured at 200-400 ms for twenty at once on colima. Once
// claimed the sandbox is the caller's, and its idle policy is the ordinary one.

// Pin keeps a sandbox from being idled (pinned) or releases it. Unpinning restarts its idle
// clock, so the window runs from the moment it was handed out, not from when it was made.
func (d *daemon) Pin(sandbox string, pinned bool) {
	d.pinnedMu.Lock()

	if d.pinned == nil {
		d.pinned = map[string]bool{}
	}

	if pinned {
		d.pinned[sandbox] = true
	} else {
		delete(d.pinned, sandbox)
	}
	d.pinnedMu.Unlock()

	d.mu.Lock()
	for _, u := range d.units {
		if u.sandbox == sandbox {
			u.pinned.Store(pinned)

			if !pinned {
				u.touch()
			}
		}
	}
	d.mu.Unlock()
}

func (d *daemon) isPinned(sandbox string) bool {
	d.pinnedMu.RLock()
	defer d.pinnedMu.RUnlock()

	return d.pinned[sandbox]
}
