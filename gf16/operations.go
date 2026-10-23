package gf16

import (
	"errors"
	"runtime"
)

const (
	// OpAdd accumulates Coeff*Src into Dst, in prepared layout.
	OpAdd uint16 = iota
	// OpScale overwrites Dst with Coeff*Src. Source and destination may alias.
	OpScale
	// OpClear zeros Dst. Src and Coeff are ignored.
	OpClear
	// OpButterfly maps (Dst,Src) to (Dst+c*Src, Dst+(c+1)*Src).
	// The two buffers must be distinct and nonoverlapping.
	OpButterfly
	// OpInverseButterfly reverses OpButterfly with the same coefficient.
	OpInverseButterfly
)

// Operation is one region operation. Its fixed-width layout matches the C
// batch executor so a transform crosses cgo once per batch, not per butterfly.
type Operation struct {
	Dst, Src    uint32
	Coeff, Kind uint16
}

// BufferSet registers prepared buffers for batched region arithmetic. Keep
// buffers and their backing arrays unchanged until Close. Concurrent Apply
// calls require distinct contexts and disjoint destinations/sources; Close
// must wait for all calls. Contexts must have the registering context's layout.
// The cgo backend pins the buffers until Close. Always defer Close immediately.
type BufferSet struct {
	count, size int
	rows        [][]byte
	method      string
	apply       func(*Context, []Operation)
	close       func()
}

func NewBufferSet(ctx *Context, rows [][]byte) (*BufferSet, error) {
	if ctx == nil {
		return nil, errors.New("gf16: nil context")
	}
	if len(rows) == 0 {
		return nil, errors.New("gf16: empty buffer set")
	}
	for _, row := range rows {
		if len(row) != ctx.BufSize() {
			return nil, errors.New("gf16: buffer size mismatch")
		}
	}
	return newBufferSet(ctx, rows)
}

func (s *BufferSet) Apply(ctx *Context, ops []Operation) error {
	if s.close == nil {
		return errors.New("gf16: closed buffer set")
	}
	if ctx == nil {
		return errors.New("gf16: nil context")
	}
	if ctx.BufSize() != s.size || ctx.MethodName() != s.method {
		return errors.New("gf16: buffer layout mismatch")
	}
	for _, op := range ops {
		if op.Kind > OpInverseButterfly || uint64(op.Dst) >= uint64(s.count) || (op.Kind != OpClear && uint64(op.Src) >= uint64(s.count)) {
			return errors.New("gf16: invalid region operation")
		}
		if op.Kind >= OpButterfly && op.Dst == op.Src {
			return errors.New("gf16: aliased butterfly")
		}
	}
	if len(ops) > 0 {
		s.apply(ctx, ops)
		runtime.KeepAlive(ctx)
		runtime.KeepAlive(s)
	}
	return nil
}

func (s *BufferSet) Close() {
	if s.close != nil {
		s.close()
		s.close = nil
		s.apply = nil
		s.rows = nil
	}
}

// NewBufferWorkspace owns a bank of prepared buffers until Close. On SIMD
// builds the bulk bank lives outside the Go heap to avoid GC/scavenger work
// during transforms. Do not retain Buffers or use them after Close.
func NewBufferWorkspace(ctx *Context, count int) (*BufferSet, error) {
	if ctx == nil || count <= 0 {
		return nil, errors.New("gf16: invalid buffer workspace")
	}
	// Include row descriptors and alignment slack before either backend allocates.
	limit := int(^uint(0) >> 1)
	if count > (limit-4096)/max(ctx.BufSize()+4096, 24) {
		return nil, errors.New("gf16: workspace too large")
	}
	return newBufferWorkspace(ctx, count)
}
func (s *BufferSet) Buffers() [][]byte {
	if s.close == nil {
		return nil
	}
	return s.rows
}
func (c *Context) operationScratch() []byte {
	if c.batchScratch == nil {
		c.batchScratch = c.NewBuffer()
	}
	return c.batchScratch
}
