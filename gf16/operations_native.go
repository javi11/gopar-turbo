package gf16

import (
	"crypto/subtle"
	"errors"
)

func newBufferSet(ctx *Context, rows [][]byte) (*BufferSet, error) {
	for _, row := range rows {
		if addr(row)%uintptr(ctx.align) != 0 {
			return nil, errors.New("gf16: unaligned prepared buffer")
		}
	}
	s := &BufferSet{count: len(rows), size: ctx.BufSize(), method: ctx.MethodName(), rows: rows}
	s.apply = func(c *Context, ops []Operation) { c.applyOperations(rows, ops) }
	s.close = func() {}
	return s, nil
}

func newBufferWorkspace(ctx *Context, count int) (*BufferSet, error) {
	return newBufferSet(ctx, ctx.NewBuffers(count))
}

// applyOperations executes ops over rows in order.
func (c *Context) applyOperations(rows [][]byte, ops []Operation) {
	if c.k == nil {
		c.applyOperationsPureGo(rows, ops)
		return
	}
	size := c.bufSize
	for i := 0; i < len(ops); {
		op := ops[i]
		run := coalescedRun(rows, size, ops[i:])
		bytes := run * size
		dst := &rows[op.Dst][0]
		switch op.Kind {
		case OpAdd:
			src := &rows[op.Src][0]
			if op.Coeff == 1 {
				c.k.xor(dst, src, bytes)
			} else if op.Coeff != 0 {
				c.k.mulAdd(dst, src, bytes, op.Coeff)
			}
		case OpScale:
			c.scale(rows[op.Dst], rows[op.Src], op.Coeff)
		case OpButterfly:
			src := &rows[op.Src][0]
			if op.Coeff != 0 {
				c.k.mulAdd(dst, src, bytes, op.Coeff)
			}
			c.k.xor(src, dst, bytes)
		case OpInverseButterfly:
			src := &rows[op.Src][0]
			c.k.xor(src, dst, bytes)
			if op.Coeff != 0 {
				c.k.mulAdd(dst, src, bytes, op.Coeff)
			}
		case OpClear:
			for r := 0; r < run; r++ {
				clear(rows[int(op.Dst)+r])
			}
		}
		i += run
	}
}

// scale sets dst = coeff*src with snapshot semantics for partial overlap.
func (c *Context) scale(dst, src []byte, coeff uint16) {
	d, s := addr(dst), addr(src)
	size := uintptr(len(dst))
	if d == s || d+size <= s || s+size <= d {
		c.k.mul(&dst[0], &src[0], len(dst), coeff)
		return
	}
	scratch := c.operationScratch()
	c.k.mul(&scratch[0], &src[0], len(dst), coeff)
	copy(dst, scratch)
}

// coalescedRun returns how many consecutive operations can be merged into one
// kernel call. A bank of prepared buffers laid out back to back is itself a
// valid larger prepared region, so operations of the same kind and
// coefficient on adjacent rows collapse into a single longer pass. Only
// independent operations merge: overlapping source and destination ranges
// must keep their sequential semantics. OpScale is never merged.
func coalescedRun(rows [][]byte, size int, ops []Operation) int {
	op := ops[0]
	if op.Kind == OpScale {
		return 1
	}
	run := 1
	for run < len(ops) {
		next := ops[run]
		if next.Kind != op.Kind || next.Coeff != op.Coeff ||
			next.Dst != op.Dst+uint32(run) ||
			addr(rows[next.Dst]) != addr(rows[op.Dst])+uintptr(run*size) {
			break
		}
		if op.Kind != OpClear {
			if next.Src != op.Src+uint32(run) ||
				addr(rows[next.Src]) != addr(rows[op.Src])+uintptr(run*size) {
				break
			}
			d, s := addr(rows[op.Dst]), addr(rows[op.Src])
			bytes := uintptr((run + 1) * size)
			if d != s && d < s+bytes && s < d+bytes {
				break
			}
		}
		run++
	}
	return run
}

// applyOperationsPureGo is the generic-path executor.
func (c *Context) applyOperationsPureGo(rows [][]byte, ops []Operation) {
	scratch := c.operationScratch()
	for _, op := range ops {
		switch op.Kind {
		case OpAdd:
			if op.Coeff == 1 {
				subtle.XORBytes(rows[op.Dst], rows[op.Dst], rows[op.Src])
			} else if op.Coeff != 0 {
				c.MulAdd(rows[op.Dst], rows[op.Src], op.Coeff)
			}
		case OpScale:
			c.Mul(scratch, rows[op.Src], op.Coeff)
			copy(rows[op.Dst], scratch)
		case OpButterfly:
			c.MulAdd(rows[op.Dst], rows[op.Src], op.Coeff)
			subtle.XORBytes(rows[op.Src], rows[op.Src], rows[op.Dst])
		case OpInverseButterfly:
			subtle.XORBytes(rows[op.Src], rows[op.Src], rows[op.Dst])
			c.MulAdd(rows[op.Dst], rows[op.Src], op.Coeff)
		case OpClear:
			clear(rows[op.Dst])
		}
	}
}
