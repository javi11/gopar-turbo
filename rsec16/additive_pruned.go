package rsec16

import "github.com/javi11/gopar-turbo/gf16"

// prunedFFT specializes the transform to known nonzero input rows (inverse)
// or requested output rows (forward). The caller mask is never modified.
func (b *additiveBasis) prunedFFT(n int, inverse bool, mask []bool) regionProgram {
	return b.prunedFFTDepth(n, additiveLog(n), inverse, false, mask)
}

func (b *additiveBasis) prunedFFTDepth(n, levels int, inverse, transpose bool, mask []bool) regionProgram {
	return b.prunedFFTCoset(n, levels, inverse, transpose, mask, 0)
}
func (b *additiveBasis) prunedFFTCoset(n, levels int, inverse, transpose bool, mask []bool, offset int) regionProgram {
	active := append([]bool(nil), mask...)
	stages := make([][]gf16.Operation, levels)
	// Both analyses traverse low levels first: forward liveness works backward
	// from outputs, while inverse zero propagation follows execution order.
	for i := 0; i < levels; i++ {
		half := 1 << i
		var ops []gf16.Operation
		for base := 0; base < n; base += 2 * half {
			c := uint16(evalLinear(b.linear[i], b.points[base+offset]).Times(b.inverse[i]))
			for r := 0; r < half; r++ {
				l, h := base+r, base+half+r
				left, right := active[l], active[h]
				if inverse {
					if left || right {
						dst, src := l, h
						if transpose {
							dst, src = h, l
						}
						ops = append(ops, gf16.Operation{Dst: uint32(dst), Src: uint32(src), Coeff: c, Kind: gf16.OpInverseButterfly})
					}
					active[l] = left && c != 1 || right && c != 0
					active[h] = left || right
					if transpose {
						active[l] = left || right
						active[h] = left && c != 0 || right && c != 1
					}
				} else {
					switch {
					case left && right:
						ops = append(ops, gf16.Operation{Dst: uint32(l), Src: uint32(h), Coeff: c, Kind: gf16.OpButterfly})
					case left:
						if c != 0 {
							ops = append(ops, gf16.Operation{Dst: uint32(l), Src: uint32(h), Coeff: c, Kind: gf16.OpAdd})
						}
					case right:
						if c == 1 {
							ops = append(ops, gf16.Operation{Dst: uint32(h), Src: uint32(l), Coeff: 1, Kind: gf16.OpScale})
						} else {
							if c != 0 {
								ops = append(ops, gf16.Operation{Dst: uint32(h), Src: uint32(h), Coeff: c ^ 1, Kind: gf16.OpScale})
							}
							ops = append(ops, gf16.Operation{Dst: uint32(h), Src: uint32(l), Coeff: 1, Kind: gf16.OpAdd})
						}
					}
					active[l] = left || right
					active[h] = left && c != 0 || right && c != 1
				}
			}
		}
		stages[i] = ops
	}
	local := min(levels, 7)
	fused := make([][]gf16.Operation, n/(1<<local))
	for step := 0; step < local; step++ {
		i := local - 1 - step
		if inverse {
			i = step
		}
		for _, op := range stages[i] {
			tile := int(op.Dst) >> local
			fused[tile] = append(fused[tile], op)
		}
	}
	var compact [][]gf16.Operation
	for _, ops := range fused {
		if len(ops) > 0 {
			compact = append(compact, ops)
		}
	}
	var program regionProgram
	if inverse {
		program = append(program, compact)
	}
	for step := 0; step < levels-local; step++ {
		i := levels - 1 - step
		if inverse {
			i = local + step
		}
		ops := stages[i]
		// Keep each butterfly's dependent scale/add pair in the same task.
		// A full stage is split only at a new destination pair.
		var tasks [][]gf16.Operation
		start := 0
		for j := 1; j < len(ops); j++ {
			if j-start >= 512 && ops[j].Dst != ops[j-1].Dst {
				tasks = append(tasks, ops[start:j])
				start = j
			}
		}
		if start < len(ops) {
			tasks = append(tasks, ops[start:])
		}
		program = append(program, tasks)
	}
	if !inverse {
		program = append(program, compact)
	}
	return program
}
