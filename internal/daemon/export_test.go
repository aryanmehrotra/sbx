package daemon

// SetScope fences a daemon built with New, as --only does, for tests outside the package.
func (d *daemon) SetScope(s Scope) { d.scope = s }
