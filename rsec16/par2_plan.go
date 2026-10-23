package rsec16

import (
	"errors"

	"github.com/javi11/gopar-turbo/gf16"
	"github.com/javi11/gopar-turbo/gf2p16"
)

// PAR2ReconstructionPlan streams survivor rows followed by selected recovery
// exponents into missing rows. Consecutive deep repairs use an additive-field
// transform; smaller or irregular systems retain matrix reconstruction. A plan
// is not safe for concurrent Fold calls. Always Close it to release workspace.
type PAR2ReconstructionPlan struct {
	matrix                           gf2p16.Matrix
	transform                        *par2Transform
	inputs, outputs, workers, budget int
	closed                           bool
}

// NewPAR2ReconstructionPlan accepts a partition of data rows and one recovery
// exponent per missing row. memoryBudget covers bulk folding buffers, not
// scalar plans or matrix metadata; zero selects 512 MiB.
func NewPAR2ReconstructionPlan(n int, available, missing, exponents []int, workers, memoryBudget int) (*PAR2ReconstructionPlan, error) {
	if n <= 0 || n > len(generators) || len(available)+len(missing) != n || len(missing) == 0 || len(exponents) != len(missing) {
		return nil, errors.New("invalid PAR2 reconstruction dimensions")
	}
	seen := make([]bool, n)
	for _, rows := range [][]int{available, missing} {
		for _, row := range rows {
			if row < 0 || row >= n || seen[row] {
				return nil, errors.New("invalid PAR2 data row partition")
			}
			seen[row] = true
		}
	}
	for _, e := range exponents {
		if e < 0 || e > 65535 {
			return nil, errors.New("parity exponent out of range")
		}
	}
	if memoryBudget <= 0 {
		memoryBudget = 512 << 20
	}
	p := &PAR2ReconstructionPlan{inputs: n, outputs: len(missing), workers: max(1, workers), budget: memoryBudget}
	_, consecutive := consecutiveStart(exponents)
	if consecutive && len(missing) >= 1024 && n*len(missing) >= 8<<20 && memoryBudget >= 64<<20 {
		p.transform = newPAR2Transform(available, missing, exponents, p.workers)
		return p, nil
	}
	if consecutive && len(missing) >= 128 {
		p.matrix, _ = par2LagrangeMatrix(n, available, missing, exponents)
		return p, nil
	}
	c, err := NewCoderPAR2VandermondeRows(n, exponents, p.workers)
	if err != nil {
		return nil, err
	}
	used := make([]int, len(exponents))
	for i := range used {
		used[i] = i
	}
	p.matrix, err = c.ReconstructionMatrix(available, missing, used)
	if err != nil {
		return nil, err
	}
	return p, nil
}

// FoldChunkSize estimates the chunk size for bulk matrix-fold buffers. The
// budget excludes matrix/scanner/native metadata and permits minimum alignment
// overhead for very small values. A zero budget selects 512 MiB.
func FoldChunkSize(budget, sliceSize, outputs int) int {
	if outputs <= 0 {
		return sliceSize
	}
	if budget <= 0 {
		budget = 512 << 20
	}
	buffers := outputs + 2
	if gf16.Accelerated() {
		buffers = 2*outputs + 49
	}
	per := budget / buffers
	if gf16.Accelerated() {
		per -= min(4096, per/8)
	}
	return min(sliceSize, max(2, per-per%2))
}

// ChunkSize chooses a byte range for the selected workspace and memory budget.
func (p *PAR2ReconstructionPlan) ChunkSize(sliceSize int) (int, error) {
	if p.closed {
		return 0, errors.New("closed PAR2 reconstruction plan")
	}
	if sliceSize <= 0 || sliceSize%2 != 0 {
		return 0, errors.New("invalid slice size")
	}
	if p.transform == nil {
		return FoldChunkSize(p.budget, sliceSize, p.outputs), nil
	}
	const stripe = transformStripeSize
	rows := 2*p.transform.degree + 3
	ctx, err := gf16.NewContext(stripe)
	if err != nil {
		return 0, err
	}
	preparedStripe := ctx.BufSize()
	ctx.Close()
	workers := min(p.workers, 1+(sliceSize-1)/stripe, max(1, p.budget/(rows*preparedStripe+(p.inputs+p.outputs)*stripe)))
	p.transform.workers = workers
	if workers > 1 && sliceSize > stripe {
		candidate := (p.budget - workers*rows*preparedStripe) / (p.inputs + p.outputs)
		// Avoid overlapping disk pages between successive repair passes.
		// This matters for positional writes into copy-on-write file clones.
		if candidate < sliceSize && candidate >= 4096 {
			candidate = candidate / 4096 * 4096
		}
		return min(sliceSize, max(2, candidate&^1)), nil
	}
	candidate := min(sliceSize, max(2, (p.budget/(rows+p.outputs)-128)&^1))
	for {
		ctx, err := gf16.NewContext(candidate)
		if err != nil {
			return 0, err
		}
		bytes := int64(rows)*int64(ctx.BufSize()) + int64(p.outputs)*int64(candidate)
		stride := ctx.Stride()
		ctx.Close()
		if bytes <= int64(p.budget) || candidate <= 2 {
			return candidate, nil
		}
		candidate = max(2, (candidate-stride)&^1)
	}
}

// Fold fully overwrites out. next fills a survivor/recovery input range; it
// must be concurrency-safe for matrix plans. Transform plans read each input
// exactly once per byte range and reuse their workspace across calls.
func (p *PAR2ReconstructionPlan) Fold(size int, next func(int, []byte) error, out [][]byte) error {
	if p.closed {
		return errors.New("closed PAR2 reconstruction plan")
	}
	if size <= 0 || size%2 != 0 || len(out) != p.outputs || next == nil {
		return errors.New("invalid reconstruction buffers")
	}
	for _, row := range out {
		if len(row) != size {
			return errors.New("invalid reconstruction output size")
		}
	}
	if p.transform != nil {
		return p.transform.fold(size, next, out)
	}
	return FoldInputsParallel(p.matrix, p.inputs, size, next, out, p.workers)
}
func (p *PAR2ReconstructionPlan) Close() {
	if !p.closed {
		if p.transform != nil {
			p.transform.close()
		}
		p.closed = true
	}
}
