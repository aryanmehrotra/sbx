//go:build !unix

package fc

func allocated(string) (int64, bool) { return 0, false }
