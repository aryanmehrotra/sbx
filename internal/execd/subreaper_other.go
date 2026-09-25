//go:build unix && !linux

package execd

import "errors"

// becomeSubreaper has no equivalent outside Linux; execd then reaps only when it is PID 1.
func becomeSubreaper() error {
	return errors.New("child subreaper is Linux-only")
}
