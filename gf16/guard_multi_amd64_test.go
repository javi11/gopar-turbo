//go:build unix

package gf16

import (
	"bytes"
	"math/rand"
	"testing"

	"github.com/javi11/gopar-turbo/gf2p16"
)

// TestGuardPageMulAddMultiSkips runs the amd64 multi-source driver's skip
// paths (zero coefficient, source aliasing dst, repeated sources) on buffers
// that end, or start, at a guard page.
func TestGuardPageMulAddMultiSkips(t *testing.T) {
	for _, k := range kernelVariants {
		t.Run(k.name, func(t *testing.T) {
			rng := rand.New(rand.NewSource(11))
			arena := newGuardArena(t)
			defer arena.release()
			for _, end := range []bool{true, false} {
				for blocks := 1; blocks <= 4; blocks++ {
					n := blocks * k.stride
					plain := func(buf []byte) []byte {
						out := make([]byte, n)
						k.finish(&out[0], &buf[0], n)
						return out
					}
					a, b := arena.alloc(n, end), arena.alloc(n, end)
					dst := arena.alloc(n, end)
					rng.Read(a)
					rng.Read(b)
					rng.Read(dst)
					orig := plain(dst)
					// slot: 0 a, 1 zero coefficient, 2 dst, 3 a again, 4 dst
					// again, 5 b, 6 zero coefficient on dst, 7 b again.
					ptrs := srcPtrs{&a[0], &b[0], &dst[0], &a[0], &dst[0], &b[0], &dst[0], &b[0]}
					c := coefList{0x1234, 0, 0x00ff, 0x8000, 0x0f0f, 0xffff, 0, 3}
					srcPlain := map[*byte][]byte{&a[0]: plain(a), &b[0]: plain(b), &dst[0]: orig}
					want := append([]byte(nil), orig...)
					for i := range ptrs {
						gf2p16.MulAndAddByteSliceLE(gf2p16.T(c[i]), srcPlain[ptrs[i]], want)
					}
					k.mulAddMulti(&dst[0], &ptrs, &c, len(ptrs), n)
					if !bytes.Equal(plain(dst), want) {
						t.Fatalf("end=%v n=%d: mismatch", end, n)
					}
				}
				arena.release()
			}
		})
	}
}
