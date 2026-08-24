//go:build darwin

package main

import "syscall"

// peakRSS returns the process's maximum resident set size in bytes. Darwin
// reports ru_maxrss in bytes.
func peakRSS() int64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	return int64(ru.Maxrss)
}
