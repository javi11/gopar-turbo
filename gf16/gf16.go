// Package gf16 provides GF(2^16) region multiply operations for PAR2
// Reed-Solomon coding. It is pure Go plus Go assembly (no cgo, no C
// toolchain): at init it selects, by GOARCH and CPU features, the fastest
// of these kernels, or the portable pure-Go fallback built on gf2p16:
//
//   - arm64: "CLMul (NEON)" (PMULL, every arm64 CPU) and "CLMul (SHA3)"
//     (the same with EOR3, preferred where available);
//   - amd64: "Shuffle (SSSE3)" and "Shuffle (AVX2)" (PSHUFB nibble tables,
//     built inline from the coefficient); with the gf16unverified
//     build tag also "Affine2x (GFNI+AVX2)" and "Affine2x (GFNI+AVX512)",
//     which have not been run on real GFNI hardware by the author and are
//     therefore never compiled into default builds;
//   - everything else: the pure-Go path.
//
// The kernels are ports of the algorithms of ParPar (see LICENSE.parpar).
//
// # Forcing a kernel
//
// The environment variable GOPAR_GF16_KERNEL, read once at program start,
// overrides the default choice. Its value is either "purego" or the exact
// name Context.MethodName reports for a kernel (for example "CLMul (NEON)").
// An unknown name, or a kernel this CPU cannot run, is ignored and the
// default is kept; it never causes a failure. This lets the whole stack
// (rsec16, par2) be exercised end to end with each kernel.
//
// Adding an architecture: see the "Adding an architecture" notes in kernel.go.
//
// Usage pattern (streaming fold or matrix apply):
//
//	ctx, _ := gf16.NewContext(sliceSize)
//	defer ctx.Close()
//	buf := ctx.NewBuffer()
//	ctx.Prepare(buf, slice)           // once per input
//	ctx.MulAdd(acc, buf, coeff)       // any number of times
//	ctx.Finish(acc, out)              // once per output, at the end
//
// A Context is NOT safe for concurrent Mul/MulAdd/MulAddMulti calls;
// use one Context per goroutine. Prepare and Finish are stateless and
// may be called concurrently on distinct buffers.
package gf16

import (
	"errors"
	"unsafe"
)

var errBadSliceSize = errors.New("gf16: slice size must be positive and even")

// alignedSlice returns a length-size slice whose backing array start is
// aligned to align (a power of two).
func alignedSlice(size, align int) []byte {
	buf := make([]byte, size+align)
	off := int(uintptr(unsafe.Pointer(&buf[0])) & uintptr(align-1))
	if off != 0 {
		off = align - off
	}
	return buf[off : off+size : off+size]
}

func checkSliceSize(sliceSize int) error {
	if sliceSize <= 0 || sliceSize%2 != 0 {
		return errBadSliceSize
	}
	return nil
}

func bufferBank(size, align, count int) [][]byte {
	if count <= 0 {
		panic("gf16: invalid buffer count")
	}
	stride := (size + align - 1) / align * align
	if stride > int(^uint(0)>>1)/count {
		panic("gf16: buffer bank too large")
	}
	bank := alignedSlice(stride*count, align)
	rows := make([][]byte, count)
	for i := range rows {
		rows[i] = bank[i*stride : i*stride+size : i*stride+size]
	}
	return rows
}
