//go:build linux

package phpfpm

import (
	"bytes"
	"os"
	"strconv"
)

// isZombie reports whether pid has exited and is waiting to be collected.
//
// The state is the field after the comm field in /proc/<pid>/stat, and comm is
// the executable name in parentheses — which may itself contain spaces and
// parentheses, so it is found from the LAST ')' rather than by splitting.
func isZombie(pid int) bool {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		// No /proc entry to read is not evidence of a zombie; the caller's
		// signal check has already spoken.
		return false
	}

	i := bytes.LastIndexByte(data, ')')
	if i < 0 || i+2 >= len(data) {
		return false
	}

	return data[i+2] == 'Z'
}
