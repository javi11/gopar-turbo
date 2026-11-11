package gf16

import (
	"fmt"
	"math/rand"
	"testing"
)

// BenchmarkSetupCost isolates the per-call coefficient setup (nibble table
// construction) by running kernels on small regions, where the setup is a
// large part of the work. Every iteration uses fresh coefficients so no
// coefficient-keyed cache could help. n is the number of sources of one
// MulAdd (n=1) or MulAddMulti call.
func BenchmarkSetupCost(b *testing.B) {
	for _, k := range testPaths() {
		for _, size := range []int{512, 4 << 10, 64 << 10, 128 << 10} {
			for _, n := range []int{1, 8, 16} {
				b.Run(fmt.Sprintf("%s/size=%d/n=%d", pathName(k), size, n), func(b *testing.B) {
					benchSetup(b, k, size, n)
				})
			}
		}
	}
}

func benchSetup(b *testing.B, k *kernel, size, n int) {
	ctx := mustContext(b, size, k)
	rng := rand.New(rand.NewSource(7))
	raw := make([]byte, size)
	srcs := make([][]byte, n)
	for i := range srcs {
		rng.Read(raw)
		srcs[i] = ctx.NewBuffer()
		ctx.Prepare(srcs[i], raw)
	}
	dst := ctx.NewBuffer()
	const nCoeffs = 1024
	coeffs := make([]uint16, nCoeffs+n)
	for i := range coeffs {
		coeffs[i] = uint16(rng.Intn(1<<16-2)) + 2
	}
	b.SetBytes(int64(n * size))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cs := coeffs[i%nCoeffs : i%nCoeffs+n]
		if n == 1 {
			ctx.MulAdd(dst, srcs[0], cs[0])
		} else {
			ctx.MulAddMulti(dst, srcs, 0, ctx.BufSize(), cs)
		}
	}
}

// BenchmarkSmallLengths compares every code path (the pure-Go path included)
// at tiny region sizes through Context.MulAdd (n=1) and Context.MulAddMulti
// (n=8), where per-call overhead dominates.
func BenchmarkSmallLengths(b *testing.B) {
	for _, size := range []int{64, 512, 4 << 10, 64 << 10} {
		for _, n := range []int{1, 8} {
			for _, k := range testPaths() {
				b.Run(fmt.Sprintf("size=%d/n=%d/%s", size, n, pathName(k)), func(b *testing.B) {
					benchSetup(b, k, size, n)
				})
			}
		}
	}
}
