package gf16

import (
	"bytes"
	"testing"

	"github.com/javi11/gopar-turbo/gf2p16"
)

// TestKernelMulAllCoefficients runs Context.Mul with every one of the 65536
// coefficients on a region whose words are n<<4k for all nibble positions k
// and values n (plus the words' complements), so each entry of a nibble-table
// kernel's tables is exercised, and compares with the gf2p16 reference.
func TestKernelMulAllCoefficients(t *testing.T) {
	var in [128]byte
	for k := 0; k < 4; k++ {
		for n := 0; n < 16; n++ {
			w := uint16(n) << (4 * k)
			i := 2 * (16*k + n)
			in[i], in[i+1] = byte(w), byte(w>>8)
		}
	}
	forEachPath(t, func(t *testing.T, k *kernel) {
		ctx := mustContext(t, len(in), k)
		src, dst := ctx.NewBuffer(), ctx.NewBuffer()
		ctx.Prepare(src, in[:])
		got, want := make([]byte, len(in)), make([]byte, len(in))
		for c := 0; c < 1<<16; c++ {
			ctx.Mul(dst, src, uint16(c))
			ctx.Finish(dst, got)
			gf2p16.MulByteSliceLE(gf2p16.T(c), in[:], want)
			if !bytes.Equal(got, want) {
				t.Fatalf("c=%#x: got %x want %x", c, got, want)
			}
		}
	})
}
