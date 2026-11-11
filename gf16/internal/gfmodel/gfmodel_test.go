package gfmodel

import (
	"testing"

	"github.com/javi11/gopar-turbo/gf2p16"
)

// TestShuffleTableAllCoefficients checks the model table against the gf2p16
// reference for every coefficient: lo/hi byte of c*(n<<4k) for all nibbles.
func TestShuffleTableAllCoefficients(t *testing.T) {
	for c := 0; c < 1<<16; c++ {
		tbl := NewShuffleTable(uint16(c))
		var in [128]byte
		for k := 0; k < 4; k++ {
			for n := 0; n < 16; n++ {
				w := uint16(n) << (4 * k)
				in[2*(16*k+n)], in[2*(16*k+n)+1] = byte(w), byte(w>>8)
			}
		}
		var out [128]byte
		gf2p16.MulByteSliceLE(gf2p16.T(c), in[:], out[:])
		for k := 0; k < 4; k++ {
			for n := 0; n < 16; n++ {
				i := 2 * (16*k + n)
				if tbl[32*k+n] != out[i] || tbl[32*k+16+n] != out[i+1] {
					t.Fatalf("c=%#x k=%d n=%d: table %02x%02x, reference %02x%02x",
						c, k, n, tbl[32*k+16+n], tbl[32*k+n], out[i+1], out[i])
				}
			}
		}
	}
}
