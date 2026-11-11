//go:build gf16unverified

package gf16

import (
	"slices"
	"testing"

	"github.com/klauspost/cpuid/v2"
)

// TestUnverifiedAffineKernelsRequireGFNI: with the gf16unverified tag the
// Affine2x kernels are registered exactly when the CPU reports GFNI together
// with the vector extensions each needs.
func TestUnverifiedAffineKernelsRequireGFNI(t *testing.T) {
	cpu := cpuid.CPU
	want := map[string]bool{
		"Affine2x (GFNI+AVX2)":   cpu.Supports(cpuid.GFNI, cpuid.AVX2),
		"Affine2x (GFNI+AVX512)": cpu.Supports(cpuid.GFNI, cpuid.AVX512F, cpuid.AVX512BW, cpuid.AVX512VL),
	}
	for name, registered := range want {
		got := slices.ContainsFunc(kernelVariants, func(k *kernel) bool { return k.name == name })
		if got != registered {
			t.Errorf("%s: registered=%v, CPU support=%v", name, got, registered)
		}
	}
	for _, k := range kernelVariants {
		if len(k.name) >= 6 && k.name[:6] == "Affine" {
			if _, ok := want[k.name]; !ok {
				t.Errorf("unexpected affine kernel %q", k.name)
			}
		}
	}
}
