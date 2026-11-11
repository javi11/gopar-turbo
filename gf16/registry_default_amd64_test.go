//go:build !gf16unverified

package gf16

import (
	"strings"
	"testing"
)

// TestDefaultBuildRegistersNoAffineKernel: the GFNI kernels have never run on
// real GFNI hardware, so default builds must neither contain nor select them,
// whatever the CPU reports.
func TestDefaultBuildRegistersNoAffineKernel(t *testing.T) {
	for _, k := range kernelVariants {
		if strings.Contains(k.name, "Affine") {
			t.Errorf("default build registered %q", k.name)
		}
	}
	if native != nil && strings.Contains(native.name, "Affine") {
		t.Errorf("default build selected %q", native.name)
	}
	if got := affineKernels(nil); len(got) != 0 {
		t.Errorf("affineKernels returned %d kernels in a default build", len(got))
	}
}
