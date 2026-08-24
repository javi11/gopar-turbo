//go:build !darwin && !linux && !windows

package main

// peakRSS returns 0 where peak memory is not implemented, so the benchmark
// still runs and simply reports no RSS.
func peakRSS() int64 { return 0 }
