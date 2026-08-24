package rsec16

import (
	"github.com/javi11/gopar-turbo/gf16"
	"github.com/javi11/gopar-turbo/gf2p16"
)

// FoldInputs computes out[i] = sum over j of m.At(i, j) * input j.
//
// Inputs are pulled one at a time through next, which must fill the supplied
// buffer with input j. The buffer is reused between calls and is zeroed before
// each one, so next may fill only a prefix and need not retain the buffer.
//
// This is the streaming counterpart to Coder.applyMatrix. Where that holds
// every input at once — and, on the gf16 backend, a prepared copy of every
// input as well — this keeps only the output accumulators resident. Peak
// memory is proportional to len(out) rather than to numInputs, which is what
// lets a caller reconstruct a set far larger than memory.
//
// Every row of out is fully overwritten and must be sliceSize bytes long.
//
// numGoroutines is accepted for symmetry with the rest of the package but is
// not yet used: the fold is bounded by the cost of fetching inputs, and a
// gf16.Context is not safe for concurrent Mul calls, so parallelising would
// need one context per goroutine. Revisit if profiling shows it matters.
func FoldInputs(m gf2p16.Matrix, numInputs, sliceSize int, next func(j int, buf []byte) error, out [][]byte, numGoroutines int) error {
	if len(out) == 0 || numInputs == 0 {
		return nil
	}
	if gf16.Accelerated() {
		return foldInputsGF16(m, numInputs, sliceSize, next, out)
	}
	return foldInputsPureGo(m, numInputs, sliceSize, next, out)
}

func foldInputsGF16(m gf2p16.Matrix, numInputs, sliceSize int, next func(j int, buf []byte) error, out [][]byte) error {
	ctx, err := gf16.NewContext(sliceSize)
	if err != nil {
		return err
	}
	defer ctx.Close()

	// Accumulators live in the backend's prepared layout for the whole fold
	// and are untransformed only once, at the end.
	zero := make([]byte, sliceSize)
	accs := make([][]byte, len(out))
	for i := range out {
		accs[i] = ctx.NewBuffer()
		ctx.Prepare(accs[i], zero)
	}

	raw := make([]byte, sliceSize)
	prepared := ctx.NewBuffer()
	for j := 0; j < numInputs; j++ {
		clear(raw)
		if err := next(j, raw); err != nil {
			return err
		}
		ctx.Prepare(prepared, raw)
		for i := range out {
			ctx.MulAdd(accs[i], prepared, uint16(m.At(i, j)))
		}
	}

	for i := range out {
		ctx.Finish(accs[i], out[i])
	}
	return nil
}

// foldInputsPureGo mirrors foldInputsGF16 without the SIMD backend. The gf2p16
// helpers accumulate in place, so out doubles as the accumulator.
func foldInputsPureGo(m gf2p16.Matrix, numInputs, sliceSize int, next func(j int, buf []byte) error, out [][]byte) error {
	for i := range out {
		clear(out[i])
	}

	raw := make([]byte, sliceSize)
	for j := 0; j < numInputs; j++ {
		clear(raw)
		if err := next(j, raw); err != nil {
			return err
		}
		for i := range out {
			// out[i] ^= m[i][j] * raw. Starting from a zeroed row, folding
			// every input matches the MulByteSliceLE-then-
			// MulAndAddByteSliceLE sequence in applyMatrixSlice.
			gf2p16.MulAndAddByteSliceLE(m.At(i, j), raw, out[i])
		}
	}
	return nil
}
