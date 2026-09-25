//go:build unix

package fc

import (
	"os"
	"syscall"
)

func allocated(p string) (int64, bool) {
	st, err := os.Stat(p)
	if err != nil {
		return 0, false
	}

	s, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}

	return int64(s.Blocks) * 512, true
}
