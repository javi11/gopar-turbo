package gf16

import (
	"fmt"
	"math/rand"
	"testing"
)

// BenchmarkKernelVariants compares every code path the host can run, to pick
// the default in each architecture's init.
func BenchmarkKernelVariants(b *testing.B) {
	const size = 64 << 10
	for _, k := range testPaths() {
		for _, n := range []int{1, 4, 8} {
			b.Run(fmt.Sprintf("%s/n=%d", pathName(k), n), func(b *testing.B) {
				ctx := mustContext(b, size, k)
				rng := rand.New(rand.NewSource(5))
				raw := make([]byte, size)
				srcs := make([][]byte, n)
				coeffs := make([]uint16, n)
				for i := range srcs {
					rng.Read(raw)
					srcs[i] = ctx.NewBuffer()
					ctx.Prepare(srcs[i], raw)
					coeffs[i] = uint16(rng.Intn(1<<16-2)) + 2
				}
				dst := ctx.NewBuffer()
				b.SetBytes(int64(n * size))
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					ctx.MulAddMulti(dst, srcs, 0, ctx.BufSize(), coeffs)
				}
			})
		}
	}
}
