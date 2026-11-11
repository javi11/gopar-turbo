package gf16

import (
	"testing"

	"github.com/javi11/gopar-turbo/gf16/internal/gfmodel"
	"github.com/klauspost/cpuid/v2"
)

// TestShuffleSetupAllCoefficients checks the assembly table builders against
// the Go model (itself checked against gf2p16 in internal/gfmodel) for all
// 65536 coefficients. The destination is pre-filled so unwritten bytes show.
func TestShuffleSetupAllCoefficients(t *testing.T) {
	impls := []struct {
		name  string
		ok    bool
		setup func(t *shufTable, c uint16)
	}{
		{"SSSE3", cpuid.CPU.Supports(cpuid.SSSE3), shufSetupSSSE3},
		{"AVX2", cpuid.CPU.Supports(cpuid.AVX2), shufSetupAVX2},
	}
	for _, im := range impls {
		t.Run(im.name, func(t *testing.T) {
			if !im.ok {
				t.Skip("CPU lacks the instruction set")
			}
			var got shufTable
			for c := 0; c < 1<<16; c++ {
				for i := range got {
					got[i] = 0xa5
				}
				im.setup(&got, uint16(c))
				if want := gfmodel.NewShuffleTable(uint16(c)); got != shufTable(want) {
					t.Fatalf("c=%#x: table\n%x\nwant\n%x", c, got, want)
				}
			}
		})
	}
}
