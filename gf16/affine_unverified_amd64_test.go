//go:build gf16unverified

package gf16

import (
	"bytes"
	"math/rand"
	"testing"

	"github.com/klauspost/cpuid/v2"
)

// Layout and xor checks for the Affine2x kernels that do not need GFNI
// (only AVX2 / AVX-512BW); the multiply kernels are covered by the shared
// differential tests whenever the CPU has GFNI.
func TestAffine2xLayoutKernels(t *testing.T) {
	type impl struct {
		name           string
		ok             bool
		stride         int
		prep, fin, xor func(dst, src *byte, n int)
	}
	impls := []impl{
		{"AVX2", cpuid.CPU.Supports(cpuid.AVX2), affineStrideAVX2, aff2xPrepareAVX2, aff2xFinishAVX2, aff2xXorAVX2},
		{"AVX512", cpuid.CPU.Supports(cpuid.AVX512F, cpuid.AVX512BW), affineStrideAVX512, aff2xPrepareAVX512, aff2xFinishAVX512, aff2xXorAVX512},
	}
	rng := rand.New(rand.NewSource(10))
	for _, im := range impls {
		t.Run(im.name, func(t *testing.T) {
			if !im.ok {
				t.Skip("CPU lacks the instruction set")
			}
			for _, n := range []int{im.stride, 2 * im.stride, 3 * im.stride, 4096} {
				src := make([]byte, n)
				rng.Read(src)
				prep := make([]byte, n)
				im.prep(&prep[0], &src[0], n)
				for v := 0; v < n; v += 16 { // per lane: 8 low bytes then 8 high bytes
					for w := 0; w < 8; w++ {
						if prep[v+w] != src[v+2*w] || prep[v+8+w] != src[v+2*w+1] {
							t.Fatalf("prepare layout wrong at %d", v)
						}
					}
				}
				out := make([]byte, n)
				im.fin(&out[0], &prep[0], n)
				if !bytes.Equal(out, src) {
					t.Fatal("finish(prepare(x)) != x")
				}
				x := make([]byte, n)
				rng.Read(x)
				want := make([]byte, n)
				for i := range want {
					want[i] = x[i] ^ src[i]
				}
				im.xor(&x[0], &src[0], n)
				if !bytes.Equal(x, want) {
					t.Fatal("xor wrong")
				}
			}
		})
	}
}
