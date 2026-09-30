package procid

import (
	"os"
	"strconv"
	"strings"
)

// StartOf reads the process's start time, in clock ticks after boot, from field 22 of
// /proc/<pid>/stat. The command name (field 2) is in parentheses and may itself contain spaces
// and parentheses, so the fields are counted from the last ")".
func StartOf(pid int) (int64, bool) {
	body, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, false
	}

	s := string(body)

	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, false
	}

	fields := strings.Fields(s[i+1:]) // fields[0] is field 3, the state
	if len(fields) < 20 {
		return 0, false
	}

	start, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil || start <= 0 {
		return 0, false
	}

	return start, true
}
