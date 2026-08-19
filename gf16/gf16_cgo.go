//go:build cgo && (darwin || (linux && (amd64 || arm64)) || (windows && amd64))

package gf16

/*
#cgo CXXFLAGS: -std=c++11
#cgo darwin LDFLAGS: ${SRCDIR}/libgf16_darwin.a -lstdc++ -lm
#cgo linux,amd64 LDFLAGS: ${SRCDIR}/libgf16_linux_amd64.a -lstdc++ -lm
#cgo linux,arm64 LDFLAGS: ${SRCDIR}/libgf16_linux_arm64.a -lstdc++ -lm
#cgo windows,amd64 LDFLAGS: ${SRCDIR}/libgf16_windows_amd64.a -lstdc++ -lm
#include "bridge.h"
#include <stdlib.h>
*/
import "C"

import (
	"errors"
	"runtime"
	"unsafe"
)

// Accelerated reports whether the SIMD (cgo) backend is in use.
func Accelerated() bool { return true }

// Context wraps one ParPar Galois16Mul instance. Not safe for concurrent
// Mul/MulAdd/MulAddMulti; Prepare/Finish are safe to call concurrently.
type Context struct {
	ctx       *C.gf16_ctx_t
	sliceSize int
	bufSize   int
	stride    int
	align     int
}

func NewContext(sliceSize int) (*Context, error) {
	if err := checkSliceSize(sliceSize); err != nil {
		return nil, err
	}
	ctx := C.gf16_create(C.size_t(sliceSize))
	if ctx == nil {
		return nil, errors.New("gf16: failed to initialize ParPar context")
	}
	c := &Context{
		ctx:       ctx,
		sliceSize: sliceSize,
		bufSize:   int(C.gf16_buf_size(ctx)),
		stride:    int(C.gf16_stride(ctx)),
		align:     int(C.gf16_alignment(ctx)),
	}
	runtime.SetFinalizer(c, (*Context).Close)
	return c, nil
}

func (c *Context) MethodName() string { return C.GoString(C.gf16_method_name(c.ctx)) }

// affineAVX2BuiltIn reports whether the GFNI/AVX2 affine translation unit was
// compiled with -mgfni. See gf16_affine_avx2_built in bridge.h.
func affineAVX2BuiltIn() bool   { return C.gf16_affine_avx2_built() != 0 }
func (c *Context) BufSize() int { return c.bufSize }
func (c *Context) Stride() int  { return c.stride }

func (c *Context) NewBuffer() []byte { return alignedSlice(c.bufSize, c.align) }

func (c *Context) Prepare(dst, src []byte) {
	if len(dst) != c.bufSize || len(src) > c.sliceSize || len(src) == 0 {
		panic("gf16: Prepare buffer size mismatch")
	}
	C.gf16_prepare(c.ctx, unsafe.Pointer(&dst[0]), unsafe.Pointer(&src[0]), C.size_t(len(src)))
}

func (c *Context) Finish(buf, out []byte) {
	if len(buf) != c.bufSize || len(out) > c.sliceSize {
		panic("gf16: Finish buffer size mismatch")
	}
	C.gf16_finish(c.ctx, unsafe.Pointer(&buf[0]))
	copy(out, buf)
}

func (c *Context) Mul(dst, src []byte, coeff uint16) {
	c.checkPair(dst, src)
	C.gf16_mul(c.ctx, unsafe.Pointer(&dst[0]), unsafe.Pointer(&src[0]),
		C.size_t(len(src)), C.uint16_t(coeff))
}

func (c *Context) MulAdd(dst, src []byte, coeff uint16) {
	c.checkPair(dst, src)
	C.gf16_muladd(c.ctx, unsafe.Pointer(&dst[0]), unsafe.Pointer(&src[0]),
		C.size_t(len(src)), C.uint16_t(coeff))
}

// MulAddMulti computes dst[offset:offset+length] ^= coeffs[i]*srcs[i][offset:...]
// for all i. offset and length must be multiples of Stride().
func (c *Context) MulAddMulti(dst []byte, srcs [][]byte, offset, length int, coeffs []uint16) {
	if len(srcs) == 0 {
		return
	}
	if len(srcs) != len(coeffs) {
		panic("gf16: srcs/coeffs length mismatch")
	}
	if offset%c.stride != 0 || length%c.stride != 0 || offset+length > c.bufSize {
		panic("gf16: MulAddMulti range not stride-aligned or out of bounds")
	}
	if length == 0 {
		return
	}
	// cgo forbids passing a Go array of unpinned Go pointers; pin each
	// source buffer for the duration of the call.
	var pinner runtime.Pinner
	defer pinner.Unpin()
	ptrs := make([]unsafe.Pointer, len(srcs))
	for i, s := range srcs {
		if len(s) != c.bufSize {
			panic("gf16: MulAddMulti src size mismatch")
		}
		pinner.Pin(&s[0])
		ptrs[i] = unsafe.Pointer(&s[0])
	}
	C.gf16_muladd_multi(c.ctx, C.unsigned(len(srcs)), C.size_t(offset),
		unsafe.Pointer(&dst[0]), &ptrs[0],
		C.size_t(length), (*C.uint16_t)(&coeffs[0]))
}

func (c *Context) checkPair(dst, src []byte) {
	if len(dst) != c.bufSize || len(src) != c.bufSize {
		panic("gf16: buffer size mismatch")
	}
	if len(src)%c.stride != 0 {
		panic("gf16: length not a stride multiple")
	}
}

func (c *Context) Close() {
	if c.ctx != nil {
		runtime.SetFinalizer(c, nil)
		C.gf16_destroy(c.ctx)
		c.ctx = nil
	}
}
