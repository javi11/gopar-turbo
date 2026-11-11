package gf16

import "os"

// This file defines the small internal interface between the architecture
// neutral Context (gf16_native.go) and a SIMD implementation.
//
// # Adding an architecture
//
// Create clmul_<goarch>.go (plus *.s) that, in an init function, detects the
// CPU and calls registerKernels with the best *kernel and every other code
// path the host can run. Nothing else needs to change:
// without a registered kernel the Context falls back to the pure-Go gf2p16
// path. A prepared region is a whole number of stride-sized blocks in the
// kernel's own layout (arm64: planar low/high byte halves),
// produced by kernel.prepare from the standard little-endian 16-bit word
// array (zero padded to a multiple of stride) and undone by kernel.finish.
// A layout that is the identity needs no more than a copy. All kernel
// functions:
//
//   - take lengths that are multiples of kernel.stride and non-zero;
//   - tolerate any alignment (kernel.align is only a performance hint);
//   - allow dst to equal src exactly (in-place) but never partially overlap;
//   - are pure functions, so one kernel is shared by every Context.
//
// To prove correctness run the differential tests: they iterate over
// kernelVariants (every code path the host can run, e.g. with and without an
// instruction set extension) and compare with the gf2p16 reference.

// maxPassRegions is the most source regions one mulAddMulti call handles.
const maxPassRegions = 8

// maxStride bounds kernel.stride so partial blocks can use a stack buffer.
const maxStride = 64

type (
	srcPtrs  [maxPassRegions]*byte
	coefList [maxPassRegions]uint16
)

// kernel is one SIMD implementation of the GF(2^16) region primitives.
type kernel struct {
	// name is reported by Context.MethodName.
	name string
	// stride is the byte granularity of region lengths (a power of two).
	stride int
	// align is the preferred buffer alignment in bytes.
	align int
	// mul sets dst[0:n] = coeff*src[0:n].
	mul func(dst, src *byte, n int, coeff uint16)
	// mulAdd sets dst[0:n] ^= coeff*src[0:n].
	mulAdd func(dst, src *byte, n int, coeff uint16)
	// mulAddMulti sets dst[0:n] ^= sum over i<nsrc of coeffs[i]*srcs[i][0:n],
	// for 1 <= nsrc <= maxPassRegions. All sources are read before dst is
	// written for a given block, so dst may be one of the sources.
	mulAddMulti func(dst *byte, srcs *srcPtrs, coeffs *coefList, nsrc, n int)
	// xor sets dst[0:n] ^= src[0:n].
	xor func(dst, src *byte, n int)
	// prepare converts n bytes from the standard little-endian word layout
	// in src to the kernel's prepared layout in dst; finish is its inverse.
	// n is a non-zero multiple of stride. The layout must keep zero blocks
	// zero, make xor layout-agnostic, and not mix data across stride blocks.
	prepare, finish func(dst, src *byte, n int)
}

var (
	// native is the kernel chosen at init, or nil for the pure-Go path.
	native *kernel
	// kernelVariants lists every kernel the host can run, for tests and
	// benchmarks that must exercise each code path.
	kernelVariants []*kernel
)

// registerKernels installs best as the default kernel and records all the
// runnable variants (best included). Called from init.
func registerKernels(best *kernel, variants ...*kernel) {
	for _, k := range variants {
		if k.stride <= 0 || k.stride > maxStride || k.stride&(k.stride-1) != 0 {
			panic("gf16: kernel stride must be a power of two no larger than maxStride")
		}
	}
	native = chooseKernel(os.Getenv(kernelEnv), best, variants)
	kernelVariants = variants
}

// kernelEnv names the environment variable that forces the kernel; see the
// package documentation.
const kernelEnv = "GOPAR_GF16_KERNEL"

// pureGoKernelName forces the portable pure-Go path.
const pureGoKernelName = "purego"

// chooseKernel returns the kernel to use as the default: best, unless name is
// "purego" (nil, the pure-Go path) or exactly the name of one of variants
// (kernels this CPU cannot run are never in variants). An empty or unknown
// name is ignored.
func chooseKernel(name string, best *kernel, variants []*kernel) *kernel {
	if name == "" {
		return best
	}
	if name == pureGoKernelName {
		return nil
	}
	for _, k := range variants {
		if k.name == name {
			return k
		}
	}
	return best
}
