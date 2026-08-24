package rsec16

import (
	"sync"

	"github.com/javi11/gopar-turbo/gf16"
	"github.com/javi11/gopar-turbo/gf2p16"
)

// foldBatchSize is how many prepared inputs are folded per accumulator pass.
// Larger batches cut accumulator cache traffic (one MulAddMulti pass per
// batch instead of per input) at the cost of two batches' worth of prepared
// buffers held for the pipeline.
const foldBatchSize = 16

// FoldInputs computes out[i] = sum over j of m.At(i, j) * input j.
//
// Inputs are pulled one at a time through next, which must fill the supplied
// buffer with input j. The buffer is reused between calls and is zeroed before
// each one, so next may fill only a prefix and need not retain the buffer.
// Calls to next may run ahead of the folding (inputs are prefetched in
// batches) but remain strictly sequential — j = 0..numInputs-1, one at a
// time, from a single goroutine — so callers need no locking.
//
// This is the streaming counterpart to Coder.applyMatrix. Where that holds
// every input at once — and, on the gf16 backend, a prepared copy of every
// input as well — this keeps only the output accumulators and two small
// input batches resident. Peak memory is proportional to len(out) rather
// than to numInputs, which is what lets a caller reconstruct a set far
// larger than memory.
//
// Every row of out is fully overwritten and must be sliceSize bytes long.
func FoldInputs(m gf2p16.Matrix, numInputs, sliceSize int, next func(j int, buf []byte) error, out [][]byte, numGoroutines int) error {
	if len(out) == 0 || numInputs == 0 {
		return nil
	}
	if numGoroutines < 1 {
		numGoroutines = 1
	}
	if gf16.Accelerated() {
		return foldInputsGF16(m, numInputs, sliceSize, next, out, numGoroutines)
	}
	return foldInputsPureGo(m, numInputs, sliceSize, next, out, numGoroutines)
}

// foldUnit is one worker work item: fold a batch of prepared sources into one
// stride-aligned byte range of one accumulator. done is the current batch's
// barrier.
type foldUnit struct {
	acc    []byte
	offset int
	length int
	srcs   [][]byte
	coeffs []uint16
	done   *sync.WaitGroup
}

func foldInputsGF16(m gf2p16.Matrix, numInputs, sliceSize int, next func(j int, buf []byte) error, out [][]byte, numGoroutines int) error {
	ctx, err := gf16.NewContext(sliceSize)
	if err != nil {
		return err
	}
	defer ctx.Close()
	bufSize, stride := ctx.BufSize(), ctx.Stride()

	// One context per worker, created up front: Mul* calls share mutScratch,
	// so a context cannot serve concurrent calls (see applyMatrixGF16).
	workerCtxs := make([]*gf16.Context, numGoroutines)
	for w := range workerCtxs {
		wctx, err := gf16.NewContext(sliceSize)
		if err != nil {
			for _, c := range workerCtxs[:w] {
				c.Close()
			}
			return err
		}
		workerCtxs[w] = wctx
	}
	defer func() {
		for _, c := range workerCtxs {
			c.Close()
		}
	}()

	// Accumulators stay in the backend's prepared layout for the whole fold
	// and are untransformed once, at the end.
	zero := make([]byte, sliceSize)
	accs := make([][]byte, len(out))
	for i := range out {
		accs[i] = ctx.NewBuffer()
		ctx.Prepare(accs[i], zero)
	}

	B := foldBatchSize
	if B > numInputs {
		B = numInputs
	}

	// Two batch banks: the reader prepares bank k^1 while workers fold bank k.
	var banks [2][][]byte
	for k := range banks {
		banks[k] = make([][]byte, B)
		for b := range banks[k] {
			banks[k][b] = ctx.NewBuffer()
		}
	}

	// Split each accumulator into stride-aligned ranges so rows x ranges
	// comfortably exceeds the worker count, mirroring applyMatrixGF16.
	perRange, numRanges := calculateParallelParams(
		bufSize, (numGoroutines+len(out)-1)/len(out), stride, stride)

	// Long-lived workers on a persistent channel; the per-batch barrier is a
	// WaitGroup counting units, so workers and contexts survive across
	// batches.
	units := make(chan foldUnit)
	var workers sync.WaitGroup
	workers.Add(numGoroutines)
	for w := 0; w < numGoroutines; w++ {
		go func(wctx *gf16.Context) {
			defer workers.Done()
			for u := range units {
				wctx.MulAddMulti(u.acc, u.srcs, u.offset, u.length, u.coeffs)
				u.done.Done()
			}
		}(workerCtxs[w])
	}

	// readBank fills bank with up to B prepared inputs starting at *jp,
	// returning the count and per-row coefficient slices. It is only ever
	// running in one goroutine at a time, so next stays sequential and the
	// raw scratch buffer is safely shared.
	raw := make([]byte, sliceSize)
	nextJ := 0
	readBank := func(bank [][]byte) (int, [][]uint16, error) {
		n := 0
		base := nextJ
		for ; n < B && nextJ < numInputs; n++ {
			clear(raw)
			if err := next(nextJ, raw); err != nil {
				return 0, nil, err
			}
			ctx.Prepare(bank[n], raw)
			nextJ++
		}
		if n == 0 {
			return 0, nil, nil
		}
		coeffs := make([][]uint16, len(out))
		for i := range out {
			coeffs[i] = make([]uint16, n)
			for b := 0; b < n; b++ {
				coeffs[i][b] = uint16(m.At(i, base+b))
			}
		}
		return n, coeffs, nil
	}

	type prepared struct {
		n      int
		coeffs [][]uint16
		err    error
	}
	ready := make(chan prepared, 1)

	var foldErr error
	k := 0
	n, coeffs, err := readBank(banks[k])
	if err != nil {
		foldErr = err
	}
	for foldErr == nil && n > 0 {
		// Prefetch the other bank while workers fold this one. The two
		// banks are disjoint buffers, so the overlap is safe.
		go func(bank [][]byte) {
			pn, pc, perr := readBank(bank)
			ready <- prepared{pn, pc, perr}
		}(banks[k^1])

		var batchDone sync.WaitGroup
		for i := range accs {
			for r := 0; r < numRanges; r++ {
				offset := r * perRange
				length := perRange
				if offset+length > bufSize {
					length = bufSize - offset
				}
				if length <= 0 {
					continue
				}
				batchDone.Add(1)
				units <- foldUnit{acc: accs[i], offset: offset, length: length,
					srcs: banks[k][:n], coeffs: coeffs[i], done: &batchDone}
			}
		}
		// Barrier: every unit of this batch must land before the next batch
		// may reuse this bank's buffers.
		batchDone.Wait()

		p := <-ready
		if p.err != nil {
			foldErr = p.err
		}
		n, coeffs = p.n, p.coeffs
		k ^= 1
	}
	close(units)
	workers.Wait()
	if foldErr != nil {
		return foldErr
	}

	// Untransform accumulators into the caller's out rows, in parallel
	// (Finish is stateless and safe to share).
	var wg sync.WaitGroup
	wg.Add(len(out))
	for i := range out {
		go func(i int) {
			defer wg.Done()
			ctx.Finish(accs[i], out[i])
		}(i)
	}
	wg.Wait()
	return nil
}

// foldInputsPureGo mirrors foldInputsGF16 without the SIMD backend. The gf2p16
// helpers accumulate in place, so out doubles as the accumulator. Workers own
// disjoint output rows, so no locking is needed; a second raw buffer lets
// input j+1 be read while j is folded. next calls never overlap: each read
// starts only after the previous one's result has been received.
func foldInputsPureGo(m gf2p16.Matrix, numInputs, sliceSize int, next func(j int, buf []byte) error, out [][]byte, numGoroutines int) error {
	for i := range out {
		clear(out[i])
	}
	if numGoroutines > len(out) {
		numGoroutines = len(out)
	}

	bufs := [2][]byte{make([]byte, sliceSize), make([]byte, sliceSize)}
	type readResult struct {
		buf []byte
		err error
	}
	ready := make(chan readResult, 1)
	read := func(j int, buf []byte) {
		clear(buf)
		err := next(j, buf)
		ready <- readResult{buf, err}
	}

	go read(0, bufs[0])
	for j := 0; j < numInputs; j++ {
		r := <-ready
		if r.err != nil {
			return r.err
		}
		if j+1 < numInputs {
			// Distinct buffer from the one being folded, so the overlap
			// is safe.
			go read(j+1, bufs[(j+1)%2])
		}

		var wg sync.WaitGroup
		wg.Add(numGoroutines)
		for w := 0; w < numGoroutines; w++ {
			go func(w int) {
				defer wg.Done()
				for i := w; i < len(out); i += numGoroutines {
					// out[i] ^= m[i][j] * raw. Starting from a zeroed row,
					// folding every input matches the MulByteSliceLE-then-
					// MulAndAddByteSliceLE sequence in applyMatrixSlice.
					gf2p16.MulAndAddByteSliceLE(m.At(i, j), r.buf, out[i])
				}
			}(w)
		}
		wg.Wait()
	}
	return nil
}
