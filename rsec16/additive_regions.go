package rsec16

import (
	"math/bits"
	"sync"

	"github.com/javi11/gopar-turbo/gf16"
	"github.com/javi11/gopar-turbo/gf2p16"
)

// Each pass consists of independent tasks. Operations inside a task remain
// ordered; a barrier separates passes. This expresses transforms as batched
// region arithmetic without crossing cgo for each individual butterfly.
type regionProgram [][][]gf16.Operation

func addRegion(ops []gf16.Operation, dst, src int, c gf2p16.T) []gf16.Operation {
	if c != 0 {
		ops = append(ops, gf16.Operation{Dst: uint32(dst), Src: uint32(src), Coeff: uint16(c)})
	}
	return ops
}
func scaleRegion(ops []gf16.Operation, row int, c gf2p16.T) []gf16.Operation {
	if c != 1 {
		ops = append(ops, gf16.Operation{Dst: uint32(row), Src: uint32(row), Coeff: uint16(c), Kind: gf16.OpScale})
	}
	return ops
}

func (b *additiveBasis) fftProgram(n int, inverse bool) regionProgram {
	levels := additiveLog(n)
	const tileLog = 7
	local := min(levels, tileLog)
	emit := func(ops []gf16.Operation, base, half int, c gf2p16.T) []gf16.Operation {
		kind := gf16.OpButterfly
		if inverse {
			kind = gf16.OpInverseButterfly
		}
		for r := 0; r < half; r++ {
			ops = append(ops, gf16.Operation{Dst: uint32(base + r), Src: uint32(base + half + r), Coeff: uint16(c), Kind: kind})
		}
		return ops
	}
	var fused [][]gf16.Operation
	// Complete the small stages inside one cache tile before moving on.
	// Each task owns its whole tile, so no intermediate barrier is needed.
	for tile := 0; tile < n; tile += 1 << local {
		ops := make([]gf16.Operation, 0, (1<<local)*local)
		for step := 0; step < local; step++ {
			i := local - 1 - step
			if inverse {
				i = step
			}
			half := 1 << i
			for base := tile; base < tile+(1<<local); base += 2 * half {
				c := evalLinear(b.linear[i], b.points[base]).Times(b.inverse[i])
				ops = emit(ops, base, half, c)
			}
		}
		if len(ops) > 0 {
			fused = append(fused, ops)
		}
	}
	var p regionProgram
	if inverse && len(fused) > 0 {
		p = append(p, fused)
	}
	for step := 0; step < levels-local; step++ {
		i := levels - 1 - step
		if inverse {
			i = local + step
		}
		half := 1 << i
		var tasks [][]gf16.Operation
		for base := 0; base < n; base += 2 * half {
			c := evalLinear(b.linear[i], b.points[base]).Times(b.inverse[i])
			for start := 0; start < half; start += 512 {
				end := min(start+512, half)
				// Operands remain separated by half, not by the task's size.
				ops := make([]gf16.Operation, 0, 2*(end-start))
				kind := gf16.OpButterfly
				if inverse {
					kind = gf16.OpInverseButterfly
				}
				for r := start; r < end; r++ {
					ops = append(ops, gf16.Operation{Dst: uint32(base + r), Src: uint32(base + half + r), Coeff: uint16(c), Kind: kind})
				}
				tasks = append(tasks, ops)
			}
		}
		p = append(p, tasks)
	}
	if !inverse && len(fused) > 0 {
		p = append(p, fused)
	}
	return p
}

func (b *additiveBasis) conversionProgram(n int, inverse bool) regionProgram {
	levels := additiveLog(n)
	p := make(regionProgram, 0, levels)
	for step := 0; step < levels; step++ {
		i := levels - 1 - step
		if inverse {
			i = step
		}
		half := 1 << i
		var tasks [][]gf16.Operation
		for base := 0; base < n; base += 2 * half {
			ops := make([]gf16.Operation, 0, half*(i+1))
			for stepR := 0; stepR < half; stepR++ {
				r := half - 1 - stepR
				if inverse {
					r = stepR
				}
				d := base + half + r
				if inverse {
					ops = scaleRegion(ops, d, b.inverse[i])
				}
				for j := 0; j < i; j++ {
					ops = addRegion(ops, base+r+(1<<j), d, b.linear[i][j])
				}
				if !inverse {
					ops = scaleRegion(ops, d, b.denominator[i])
				}
			}
			if len(ops) > 0 {
				tasks = append(tasks, ops)
			}
		}
		var coalesced [][]gf16.Operation
		var small []gf16.Operation
		for _, ops := range tasks {
			if len(ops) >= 512 {
				if len(small) > 0 {
					coalesced = append(coalesced, small)
					small = nil
				}
				coalesced = append(coalesced, ops)
			} else {
				small = append(small, ops...)
				if len(small) >= 512 {
					coalesced = append(coalesced, small)
					small = nil
				}
			}
		}
		if len(small) > 0 {
			coalesced = append(coalesced, small)
		}
		tasks = coalesced
		p = append(p, tasks)
	}
	return p
}

func (b *additiveBasis) derivativeProgram(n int) regionProgram {
	levels := additiveLog(n)
	local := min(levels, 7)
	tileSize := 1 << local
	// Destinations with the same upper-coordinate population count are
	// independent. Every cross-tile source has one more set bit and is still
	// original. Process each small tile recursively while it is cache-resident.
	passes := make(regionProgram, levels-local+1)
	for tile := 0; tile < n/tileSize; tile++ {
		base := tile * tileSize
		var ops []gf16.Operation
		var visit func(int, int)
		visit = func(start, i int) {
			if i < 0 {
				ops = append(ops, gf16.Operation{Dst: uint32(start), Kind: gf16.OpClear})
				return
			}
			half := 1 << i
			visit(start, i-1)
			for r := 0; r < half; r++ {
				ops = addRegion(ops, start+r, start+half+r, b.derivative[i])
			}
			visit(start+half, i-1)
		}
		visit(base, local-1)
		for i := local; i < levels; i++ {
			if base&(1<<i) == 0 {
				for r := 0; r < tileSize; r++ {
					ops = addRegion(ops, base+r, base+(1<<i)+r, b.derivative[i])
				}
			}
		}
		weight := bits.OnesCount(uint(tile))
		passes[weight] = append(passes[weight], ops)
	}
	return passes
}

func runRegionProgram(set *gf16.BufferSet, contexts []*gf16.Context, p regionProgram) error {
	for _, tasks := range p {
		if len(contexts) == 1 {
			for _, ops := range tasks {
				if err := set.Apply(contexts[0], ops); err != nil {
					return err
				}
			}
			continue
		}
		workers := min(len(contexts), len(tasks))
		errs := make([]error, workers)
		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for j := w; j < len(tasks); j += workers {
					if err := set.Apply(contexts[w], tasks[j]); err != nil {
						errs[w] = err
						return
					}
				}
			}(w)
		}
		wg.Wait()
		for _, err := range errs {
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// transposeRegionProgram reverses a sequence of elementary linear operations.
// Conversion programs contain only scaling and accumulation operations.
func transposeRegionProgram(p regionProgram) regionProgram {
	result := make(regionProgram, len(p))
	for i, pass := range p {
		dst := make([][]gf16.Operation, len(pass))
		for j, task := range pass {
			ops := make([]gf16.Operation, len(task))
			for k, op := range task {
				if op.Kind == gf16.OpAdd {
					op.Dst, op.Src = op.Src, op.Dst
				}
				ops[len(task)-1-k] = op
			}
			dst[j] = ops
		}
		result[len(p)-1-i] = dst
	}
	return result
}

func serialRegionProgram(p regionProgram) regionProgram {
	var count int
	for _, pass := range p {
		for _, ops := range pass {
			count += len(ops)
		}
	}
	all := make([]gf16.Operation, 0, count)
	for _, pass := range p {
		for _, ops := range pass {
			all = append(all, ops...)
		}
	}
	return regionProgram{{all}}
}
