//go:build !linux && !darwin

package procid

// StartOf cannot tell on this platform, so records here carry the pid alone and are judged by
// it, as they were before start times were kept.
func StartOf(int) (int64, bool) { return 0, false }
