// Package gf16 provides GF(2^16) region multiply operations for PAR2
// Reed-Solomon coding. With cgo enabled it uses ParPar's SIMD kernels
// (SSE2..AVX-512/GFNI on amd64, NEON/SVE2 on arm64); otherwise it falls
// back to the pure-Go gf2p16 implementation.
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
