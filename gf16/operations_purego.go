//go:build !cgo || !(darwin || (linux && (amd64 || arm64)) || (windows && amd64))

package gf16

import "crypto/subtle"

func newBufferSet(ctx *Context, rows [][]byte) (*BufferSet, error) {
	s := &BufferSet{count: len(rows), size: ctx.BufSize(), method: ctx.MethodName(), rows: rows}
	s.apply = func(c *Context, ops []Operation) {
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
	s.close = func() {}
	return s, nil
}

func newBufferWorkspace(ctx *Context, count int) (*BufferSet, error) {
	return newBufferSet(ctx, ctx.NewBuffers(count))
}
