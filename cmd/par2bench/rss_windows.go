//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

// processMemoryCounters mirrors PROCESS_MEMORY_COUNTERS from psapi.h.
type processMemoryCounters struct {
	CB                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

var (
	psapi                    = syscall.NewLazyDLL("psapi.dll")
	procGetProcessMemoryInfo = psapi.NewProc("GetProcessMemoryInfo")
	kernel32                 = syscall.NewLazyDLL("kernel32.dll")
	procGetCurrentProcess    = kernel32.NewProc("GetCurrentProcess")
)

// peakRSS returns the process's peak working set size in bytes, which is the
// closest Windows analogue of a maximum resident set size.
func peakRSS() int64 {
	handle, _, _ := procGetCurrentProcess.Call()
	var counters processMemoryCounters
	counters.CB = uint32(unsafe.Sizeof(counters))
	ret, _, _ := procGetProcessMemoryInfo.Call(handle,
		uintptr(unsafe.Pointer(&counters)), uintptr(counters.CB))
	if ret == 0 {
		return 0
	}
	return int64(counters.PeakWorkingSetSize)
}
