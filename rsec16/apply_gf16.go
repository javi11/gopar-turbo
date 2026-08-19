package rsec16

import (
	"sync"

	"github.com/javi11/gopar-turbo/gf16"
	"github.com/javi11/gopar-turbo/gf2p16"
)

// applyMatrixGF16 computes out[i] = sum_j m[i][j]*in[j] using the gf16
// backend. Semantics match applyMatrixParallelData: every out row is fully
// overwritten. in rows must all have len(in[0]) bytes; out rows likewise.
func applyMatrixGF16(m gf2p16.Matrix, in, out [][]byte, numGoroutines int) {
	sliceSize := len(in[0])
	// One context for layout metadata and prepares (Prepare is stateless).
	ctx, err := gf16.NewContext(sliceSize)
	if err != nil {
		// Sizes are validated by callers; treat as programmer error like
		// the rest of this package does.
		panic(err)
	}
	defer ctx.Close()

	// Prepare all inputs once, in parallel.
	prepared := make([][]byte, len(in))
	var wg sync.WaitGroup
	wg.Add(len(in))
	for j := range in {
		go func(j int) {
			defer wg.Done()
			prepared[j] = ctx.NewBuffer()
			ctx.Prepare(prepared[j], in[j])
		}(j)
	}
	wg.Wait()

	// Zero-prepared accumulators for each output row.
	accs := make([][]byte, len(out))
	zero := make([]byte, sliceSize)
	for i := range out {
		accs[i] = ctx.NewBuffer()
		ctx.Prepare(accs[i], zero)
	}

	if numGoroutines < 1 {
		numGoroutines = 1
	}

	// Split work into (output row x stride-aligned data range) units.
	bufSize := ctx.BufSize()
	perRange, numRanges := calculateParallelParams(
		bufSize, (numGoroutines+len(out)-1)/len(out), ctx.Stride(), ctx.Stride())

	type unit struct{ row, offset, length int }
	units := make(chan unit)
	var workers sync.WaitGroup
	workers.Add(numGoroutines)
	for w := 0; w < numGoroutines; w++ {
		go func() {
			defer workers.Done()
			// Each worker needs its own context: Mul* calls share mutScratch.
			wctx, err := gf16.NewContext(sliceSize)
			if err != nil {
				panic(err)
			}
			defer wctx.Close()
			coeffs := make([]uint16, len(in))
			for u := range units {
				for j := range in {
					coeffs[j] = uint16(m.At(u.row, j))
				}
				wctx.MulAddMulti(accs[u.row], prepared, u.offset, u.length, coeffs)
			}
		}()
	}
	for i := range out {
		for r := 0; r < numRanges; r++ {
			offset := r * perRange
			length := perRange
			if offset+length > bufSize {
				length = bufSize - offset
			}
			if length > 0 {
				units <- unit{row: i, offset: offset, length: length}
			}
		}
	}
	close(units)
	workers.Wait()

	// Untransform accumulators into the caller's out rows, in parallel.
	wg.Add(len(out))
	for i := range out {
		go func(i int) {
			defer wg.Done()
			ctx.Finish(accs[i], out[i])
		}(i)
	}
	wg.Wait()
}
