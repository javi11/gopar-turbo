//go:build !cgo || !(darwin || (linux && (amd64 || arm64)) || (windows && amd64))

package gf16

import "github.com/javi11/gopar-turbo/gf2p16"

// Accelerated reports whether the SIMD (cgo) backend is in use.
func Accelerated() bool { return false }

// Context is the pure-Go fallback backend. The prepared layout is the
// identity (a plain copy), stride is 2 bytes (one GF(2^16) word).
type Context struct {
	sliceSize int
	bufSize   int
}

func NewContext(sliceSize int) (*Context, error) {
	if err := checkSliceSize(sliceSize); err != nil {
		return nil, err
	}
	return &Context{sliceSize: sliceSize, bufSize: sliceSize}, nil
}

func (c *Context) MethodName() string { return "gf2p16 (pure Go)" }
func (c *Context) BufSize() int       { return c.bufSize }
func (c *Context) Stride() int        { return 2 }
func (c *Context) NewBuffer() []byte  { return alignedSlice(c.bufSize, 16) }

func (c *Context) Prepare(dst, src []byte) {
	if len(dst) != c.bufSize || len(src) > c.sliceSize || len(src) == 0 {
		panic("gf16: Prepare buffer size mismatch")
	}
	n := copy(dst, src)
	clear(dst[n:])
}

func (c *Context) Finish(buf, out []byte) {
	if len(buf) != c.bufSize || len(out) > c.sliceSize {
		panic("gf16: Finish buffer size mismatch")
	}
	copy(out, buf)
}

func (c *Context) Mul(dst, src []byte, coeff uint16) {
	gf2p16.MulByteSliceLE(gf2p16.T(coeff), src, dst)
}

func (c *Context) MulAdd(dst, src []byte, coeff uint16) {
	gf2p16.MulAndAddByteSliceLE(gf2p16.T(coeff), src, dst)
}

func (c *Context) MulAddMulti(dst []byte, srcs [][]byte, offset, length int, coeffs []uint16) {
	if len(srcs) != len(coeffs) {
		panic("gf16: srcs/coeffs length mismatch")
	}
	for i, s := range srcs {
		gf2p16.MulAndAddByteSliceLE(gf2p16.T(coeffs[i]),
			s[offset:offset+length], dst[offset:offset+length])
	}
}

func (c *Context) Close() {}
