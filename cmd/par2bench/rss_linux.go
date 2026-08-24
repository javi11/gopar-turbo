//go:build linux

package main

import "syscall"

// peakRSS returns the process's maximum resident set size in bytes. Linux
// reports ru_maxrss in kilobytes.
func peakRSS() int64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	return int64(ru.Maxrss) * 1024
}
