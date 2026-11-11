package gf16

import (
	"unsafe"

	"github.com/javi11/gopar-turbo/gf2p16"
)

// Accelerated reports whether a SIMD kernel is active, as opposed to the
// pure-Go path (no kernel for this CPU, or GOPAR_GF16_KERNEL=purego).
func Accelerated() bool { return native != nil }

// Context binds a region-multiply kernel to one slice size. With a SIMD
// kernel the prepared layout is the plain word array padded to the kernel
// stride; without one it is the same layout with a stride of one word. The
// MulAddMulti packs its sources and coefficients in per-context scratch
// (ptrs, cs) and BufferSet.Apply uses batchScratch, so those are not
// reentrant: use one Context per goroutine for MulAddMulti and Apply. Mul,
// MulAdd, Prepare and Finish keep no state in the Context.
type Context struct {
	k            *kernel // nil selects the pure-Go gf2p16 path
	batchScratch []byte
	ptrs         srcPtrs // MulAddMulti packing scratch; here so calls through the kernel func values do not heap-allocate
	cs           coefList
	method       string
	sliceSize    int
	bufSize      int
	stride       int
	align        int
}

const (
	pureGoStride = 2 // one GF(2^16) word
	pureGoAlign  = 16
)

// NewContext returns a context for slices of sliceSize bytes (positive, even).
func NewContext(sliceSize int) (*Context, error) {
	return newContext(sliceSize, native)
}

// newContext is NewContext with an explicit kernel (nil for pure Go), so
// tests can force a code path.
func newContext(sliceSize int, k *kernel) (*Context, error) {
	if err := checkSliceSize(sliceSize); err != nil {
		return nil, err
	}
	if k == nil {
		return &Context{method: "gf2p16 (pure Go)", sliceSize: sliceSize, bufSize: sliceSize,
			stride: pureGoStride, align: pureGoAlign}, nil
	}
	return &Context{
		k: k, method: k.name, sliceSize: sliceSize,
		bufSize: (sliceSize + k.stride - 1) / k.stride * k.stride,
		stride:  k.stride, align: k.align,
	}, nil
}

func (c *Context) MethodName() string { return c.method }
func (c *Context) BufSize() int       { return c.bufSize }
func (c *Context) Stride() int        { return c.stride }
func (c *Context) NewBuffer() []byte  { return alignedSlice(c.bufSize, c.align) }

// NewBuffers allocates an aligned bank of equally sized prepared buffers.
func (c *Context) NewBuffers(count int) [][]byte { return bufferBank(c.bufSize, c.align, count) }

// Prepare converts src (at most sliceSize bytes) into the prepared layout in
// dst (bufSize bytes), zero-padding the tail.
func (c *Context) Prepare(dst, src []byte) {
	if len(dst) != c.bufSize || len(src) > c.sliceSize || len(src) == 0 {
		panic("gf16: Prepare buffer size mismatch")
	}
	if c.k == nil {
		n := copy(dst, src)
		clear(dst[n:])
		return
	}
	done := len(src) &^ (c.stride - 1)
	if done > 0 {
		c.k.prepare(&dst[0], &src[0], done)
	}
	if done < len(src) { // partial final block, zero padded
		var tail [maxStride]byte
		copy(tail[:], src[done:])
		c.k.prepare(&dst[done], &tail[0], c.stride)
		done += c.stride
	}
	clear(dst[done:])
}

// Finish converts a prepared buffer back to the standard layout in out. buf
// is left unchanged.
func (c *Context) Finish(buf, out []byte) {
	if len(buf) != c.bufSize || len(out) > c.sliceSize {
		panic("gf16: Finish buffer size mismatch")
	}
	if c.k == nil {
		copy(out, buf)
		return
	}
	done := len(out) &^ (c.stride - 1)
	if done > 0 {
		c.k.finish(&out[0], &buf[0], done)
	}
	if done < len(out) {
		var tail [maxStride]byte
		c.k.finish(&tail[0], &buf[done], c.stride)
		copy(out[done:], tail[:])
	}
}

// Mul sets dst = coeff*src. dst and src may be the same buffer.
func (c *Context) Mul(dst, src []byte, coeff uint16) {
	if c.k == nil {
		gf2p16.MulByteSliceLE(gf2p16.T(coeff), src, dst)
		return
	}
	c.checkPair(dst, src)
	if len(src) == 0 {
		return
	}
	c.k.mul(&dst[0], &src[0], len(src), coeff)
}

// MulAdd sets dst ^= coeff*src.
func (c *Context) MulAdd(dst, src []byte, coeff uint16) {
	if c.k == nil {
		gf2p16.MulAndAddByteSliceLE(gf2p16.T(coeff), src, dst)
		return
	}
	c.checkPair(dst, src)
	if len(src) == 0 || coeff == 0 {
		return
	}
	if coeff == 1 {
		c.k.xor(&dst[0], &src[0], len(src))
		return
	}
	c.k.mulAdd(&dst[0], &src[0], len(src), coeff)
}

// MulAddMulti computes dst[offset:offset+length] ^= coeffs[i]*srcs[i][offset:...]
// for all i. offset and length must be multiples of Stride(). Every argument
// is validated before dst is written. A source may be dst itself (the result
// is then as if all sources were read before the call).
func (c *Context) MulAddMulti(dst []byte, srcs [][]byte, offset, length int, coeffs []uint16) {
	if len(srcs) != len(coeffs) {
		panic("gf16: srcs/coeffs length mismatch")
	}
	if len(srcs) == 0 {
		return
	}
	if c.k == nil {
		c.mulAddMultiPureGo(dst, srcs, offset, length, coeffs)
		return
	}
	if offset < 0 || length < 0 || offset%c.stride != 0 || length%c.stride != 0 || offset+length > c.bufSize {
		panic("gf16: MulAddMulti range not stride-aligned or out of bounds")
	}
	if length == 0 {
		return
	}
	if len(dst) != c.bufSize {
		panic("gf16: MulAddMulti dst size mismatch")
	}
	d := &dst[offset]
	// Validate every source and collect the coefficient of those aliasing
	// dst. Passes run one after another, so an alias read in a later pass
	// would see dst already updated; by linearity all aliases fold into one
	// coefficient, applied in the first pass where dst is still unmodified.
	var self uint16
	for i, s := range srcs {
		if len(s) != c.bufSize {
			panic("gf16: MulAddMulti src size mismatch")
		}
		if &s[offset] == d {
			self ^= coeffs[i]
		}
	}
	ptrs, cs := &c.ptrs, &c.cs
	defer func() { *ptrs = srcPtrs{} }() // do not keep the caller's buffers reachable
	n := 0
	if self != 0 {
		ptrs[0], cs[0] = d, self
		n = 1
	}
	for i, s := range srcs {
		p := &s[offset]
		if coeffs[i] == 0 || p == d {
			continue
		}
		ptrs[n], cs[n] = p, coeffs[i]
		n++
		if n == maxPassRegions {
			c.k.mulAddMulti(d, ptrs, cs, n, length)
			n = 0
		}
	}
	if n > 0 {
		c.k.mulAddMulti(d, ptrs, cs, n, length)
	}
}

// mulAddMultiPureGo is MulAddMulti for the pure-Go path, with the same
// validation and aliasing semantics.
func (c *Context) mulAddMultiPureGo(dst []byte, srcs [][]byte, offset, length int, coeffs []uint16) {
	if offset < 0 || length < 0 || offset+length > len(dst) {
		panic("gf16: MulAddMulti range out of bounds")
	}
	if length == 0 {
		return
	}
	d := dst[offset : offset+length]
	var self uint16
	for i, s := range srcs {
		if len(s) < offset+length {
			panic("gf16: MulAddMulti src size mismatch")
		}
		if &s[offset] == &d[0] {
			self ^= coeffs[i]
		}
	}
	if self != 0 {
		gf2p16.MulAndAddByteSliceLE(gf2p16.T(self), d, d)
	}
	for i, s := range srcs {
		if coeffs[i] != 0 && &s[offset] != &d[0] {
			gf2p16.MulAndAddByteSliceLE(gf2p16.T(coeffs[i]), s[offset:offset+length], d)
		}
	}
}

func (c *Context) checkPair(dst, src []byte) {
	if len(dst) != len(src) {
		panic("gf16: buffer size mismatch")
	}
	if len(src)%c.stride != 0 {
		panic("gf16: length not a stride multiple")
	}
}

// Close releases the context. It is a no-op for this backend.
func (c *Context) Close() {}

// addr returns the address of the first byte of b (b must be non-empty).
func addr(b []byte) uintptr { return uintptr(unsafe.Pointer(&b[0])) }
