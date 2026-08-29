//go:build linux

package phpfpm

import (
	"bytes"
	"os"
	"strconv"
	"strings"
)

// processStartedAt is the process's start time in clock ticks since boot, from
// field 22 of /proc/<pid>/stat.
//
// Zero when it cannot be read, which the caller treats as "no opinion" rather
// than as a mismatch — refusing a reload because /proc could not be read would
// be worse than the reuse it guards against.
//
// The fields before it are found from the LAST ')' because the executable name
// sits in parentheses and may itself contain spaces and parentheses.
func processStartedAt(pid int) uint64 {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}

	i := bytes.LastIndexByte(data, ')')
	if i < 0 || i+2 >= len(data) {
		return 0
	}

	// Field 3 (state) onwards. starttime is field 22, so it is the 20th here.
	fields := strings.Fields(string(data[i+2:]))
	const startTimeOffset = 19
	if len(fields) <= startTimeOffset {
		return 0
	}

	ticks, err := strconv.ParseUint(fields[startTimeOffset], 10, 64)
	if err != nil {
		return 0
	}

	return ticks
}
