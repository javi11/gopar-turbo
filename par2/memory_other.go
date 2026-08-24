//go:build !darwin && !linux

package par2

// physicalMemory returns 0 on platforms where it is not implemented, so the
// fallback budget is used.
func physicalMemory() int64 { return 0 }
