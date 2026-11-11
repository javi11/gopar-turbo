//go:build !gf16unverified

package gf16

import "github.com/klauspost/cpuid/v2"

// affineKernels returns the GFNI kernels the CPU can run, ordered from least
// to most preferred. They are compiled only with the gf16unverified build
// tag: no GFNI-capable CPU or emulator was available to run the differential
// tests against them, so default builds never select them.
func affineKernels(*cpuid.CPUInfo) []*kernel { return nil }
