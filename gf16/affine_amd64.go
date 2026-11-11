package gf16

import (
	"github.com/javi11/gopar-turbo/gf16/internal/gfmodel"
	"github.com/klauspost/cpuid/v2"
)

// Affine2x ("GF2P8AFFINEQB") kernels. The matrix builder is plain Go in
// internal/gfmodel, tested on every host by affine_model_amd64_test.go.
//
// Prepared layout (ParPar's affine2x): per 128-bit lane, the low bytes of 8
// words (qword 0) followed by their high bytes (qword 1). Multiplying a
// word by c is a 16x16 matrix over GF(2); in 8x8 blocks M[out][in] with
// out/in in {lo, hi}. With data = [lo8 | hi8] the instruction pair
//
//	norm = affine(data, [Mll | Mhh])   -> [Mll*lo | Mhh*hi]
//	swap = affine(data, [Mhl | Mlh])   -> [Mhl*lo | Mlh*hi]
//
// gives product = norm ^ swapqwords(swap): [Mll*lo ^ Mlh*hi | Mhh*hi ^ Mhl*lo].

// Verified on real GFNI hardware in CI: the shared differential tests pick the
// kernels up through kernelVariants wherever the CPU can run them. They are
// registered only when cpuid reports GFNI plus the vector extensions each
// needs; cpuid sets the AVX/AVX-512 flags only if the OS has enabled the
// matching register state (XGETBV), so a kernel is never chosen on a system
// that cannot save its registers.

const (
	affineStrideAVX2   = 32
	affineStrideAVX512 = 64
	// Regions per pass: the vector register budget (16 ymm: 12 matrices plus
	// data and accumulators; 32 zmm: 16 matrices plus the same).
	affinePassAVX2   = 6
	affinePassAVX512 = 8
)

// Kernel entry points (affine_amd64.s). srcs/mats hold nsrc entries; the
// MulAdd kernels xor, the Mul kernels write only srcs[0]*mats[0].

//go:noescape
func aff2xMulAddAVX2(dst *byte, srcs *srcPtrs, mats *[maxPassRegions]gfmodel.AffMat, nsrc, n int)

//go:noescape
func aff2xMulAVX2(dst *byte, srcs *srcPtrs, mats *[maxPassRegions]gfmodel.AffMat, nsrc, n int)

//go:noescape
func aff2xMulAddAVX512(dst *byte, srcs *srcPtrs, mats *[maxPassRegions]gfmodel.AffMat, nsrc, n int)

//go:noescape
func aff2xMulAVX512(dst *byte, srcs *srcPtrs, mats *[maxPassRegions]gfmodel.AffMat, nsrc, n int)

//go:noescape
func aff2xPrepareAVX2(dst, src *byte, n int)

//go:noescape
func aff2xFinishAVX2(dst, src *byte, n int)

//go:noescape
func aff2xXorAVX2(dst, src *byte, n int)

//go:noescape
func aff2xPrepareAVX512(dst, src *byte, n int)

//go:noescape
func aff2xFinishAVX512(dst, src *byte, n int)

//go:noescape
func aff2xXorAVX512(dst, src *byte, n int)

func affMulAVX2(dst, src *byte, n int, coeff uint16) {
	ps, ms := srcPtrs{src}, [maxPassRegions]gfmodel.AffMat{gfmodel.AffineMatrix(coeff)}
	aff2xMulAVX2(dst, &ps, &ms, 1, n)
}

func affMulAddAVX2(dst, src *byte, n int, coeff uint16) {
	ps, ms := srcPtrs{src}, [maxPassRegions]gfmodel.AffMat{gfmodel.AffineMatrix(coeff)}
	aff2xMulAddAVX2(dst, &ps, &ms, 1, n)
}

func affMulAVX512(dst, src *byte, n int, coeff uint16) {
	ps, ms := srcPtrs{src}, [maxPassRegions]gfmodel.AffMat{gfmodel.AffineMatrix(coeff)}
	aff2xMulAVX512(dst, &ps, &ms, 1, n)
}

func affMulAddAVX512(dst, src *byte, n int, coeff uint16) {
	ps, ms := srcPtrs{src}, [maxPassRegions]gfmodel.AffMat{gfmodel.AffineMatrix(coeff)}
	aff2xMulAddAVX512(dst, &ps, &ms, 1, n)
}

func affMultiAVX2(dst *byte, srcs *srcPtrs, coeffs *coefList, nsrc, n int) {
	var ps srcPtrs
	var ms [maxPassRegions]gfmodel.AffMat
	if self := selfCoeff(dst, srcs, coeffs, nsrc); self != 0 {
		ps[0], ms[0] = dst, gfmodel.AffineMatrix(self)
		aff2xMulAddAVX2(dst, &ps, &ms, 1, n)
	}
	k := 0
	for i := 0; i < nsrc; i++ {
		if srcs[i] == dst || coeffs[i] == 0 {
			continue
		}
		ps[k], ms[k] = srcs[i], gfmodel.AffineMatrix(coeffs[i])
		if k++; k == affinePassAVX2 {
			aff2xMulAddAVX2(dst, &ps, &ms, k, n)
			k = 0
		}
	}
	if k > 0 {
		aff2xMulAddAVX2(dst, &ps, &ms, k, n)
	}
}

func affMultiAVX512(dst *byte, srcs *srcPtrs, coeffs *coefList, nsrc, n int) {
	var ps srcPtrs
	var ms [maxPassRegions]gfmodel.AffMat
	if self := selfCoeff(dst, srcs, coeffs, nsrc); self != 0 {
		ps[0], ms[0] = dst, gfmodel.AffineMatrix(self)
		aff2xMulAddAVX512(dst, &ps, &ms, 1, n)
	}
	k := 0
	for i := 0; i < nsrc; i++ {
		if srcs[i] == dst || coeffs[i] == 0 {
			continue
		}
		ps[k], ms[k] = srcs[i], gfmodel.AffineMatrix(coeffs[i])
		if k++; k == affinePassAVX512 {
			aff2xMulAddAVX512(dst, &ps, &ms, k, n)
			k = 0
		}
	}
	if k > 0 {
		aff2xMulAddAVX512(dst, &ps, &ms, k, n)
	}
}

// affineKernels returns the GFNI kernels the CPU can run, ordered from least
// to most preferred (ParPar: AVX-512 before AVX2).
func affineKernels(cpu *cpuid.CPUInfo) []*kernel {
	var ks []*kernel
	if cpu.Supports(cpuid.GFNI, cpuid.AVX2) {
		ks = append(ks, &kernel{name: "Affine2x (GFNI+AVX2)", stride: affineStrideAVX2, align: 32,
			mul: affMulAVX2, mulAdd: affMulAddAVX2, mulAddMulti: affMultiAVX2, xor: aff2xXorAVX2,
			prepare: aff2xPrepareAVX2, finish: aff2xFinishAVX2})
	}
	if cpu.Supports(cpuid.GFNI, cpuid.AVX512F, cpuid.AVX512BW, cpuid.AVX512VL) {
		ks = append(ks, &kernel{name: "Affine2x (GFNI+AVX512)", stride: affineStrideAVX512, align: 64,
			mul: affMulAVX512, mulAdd: affMulAddAVX512, mulAddMulti: affMultiAVX512, xor: aff2xXorAVX512,
			prepare: aff2xPrepareAVX512, finish: aff2xFinishAVX512})
	}
	return ks
}
