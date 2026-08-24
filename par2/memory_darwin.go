//go:build darwin

package par2

import (
	"os/exec"
	"strconv"
	"strings"
)

// physicalMemory returns total physical memory in bytes, or 0 if unknown.
func physicalMemory() int64 {
	out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0
	}
	return n
}
