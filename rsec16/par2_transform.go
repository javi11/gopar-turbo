package rsec16

import (
	"math/bits"
	"sync"
	"sync/atomic"

	"github.com/javi11/gopar-turbo/gf16"
	"github.com/javi11/gopar-turbo/gf2p16"
)

const par2FieldSize = 1 << 16
const transformStripeSize = 256

type regionWorkspace struct {
	ctx           *gf16.Context
	contexts      []*gf16.Context
	rows          [][]byte
	set           *gf16.BufferSet
	raw, finished []byte
	size          int
}

func newRegionWorkspace(size, workers, count int) (*regionWorkspace, error) {
	ctx, err := gf16.NewContext(size)
	if err != nil {
		return nil, err
	}
	w := &regionWorkspace{ctx: ctx, contexts: []*gf16.Context{ctx}, size: size, raw: make([]byte, size), finished: ctx.NewBuffer()}
	for i := 1; i < max(1, workers); i++ {
		c, err := gf16.NewContext(size)
		if err != nil {
			w.close()
			return nil, err
		}
		w.contexts = append(w.contexts, c)
	}
	w.set, err = gf16.NewBufferWorkspace(ctx, count)
	if err == nil {
		w.rows = w.set.Buffers()
	}
	if err != nil {
		w.close()
		return nil, err
	}
	return w, nil
}
func (w *regionWorkspace) close() {
	if w.set != nil {
		w.set.Close()
		w.set = nil
		w.rows = nil
	}
	for _, c := range w.contexts {
		c.Close()
	}
}
func (w *regionWorkspace) run(p regionProgram) error { return runRegionProgram(w.set, w.contexts, p) }
func (w *regionWorkspace) clear() {
	for _, row := range w.rows {
		clear(row)
	}
}

// par2Transform applies the transposed Vandermonde inverse without retaining
// a dense coefficient matrix. The workspace is reused across byte ranges.
type cosetRow struct{ index, point int }
type transformCoset struct {
	rows    []cosetRow
	program regionProgram
	scales  []gf16.Operation
}

type par2Transform struct {
	survivors, results []transformCoset

	toMoments                                                          regionProgram
	sumMoments                                                         []gf16.Operation
	degree                                                             int
	available, missing                                                 []int
	weights, scales                                                    []gf2p16.T
	multiply                                                           []gf16.Operation
	toProduct, fromProduct, forwardProduct, inverseProduct, toQuotient regionProgram
	workers                                                            int
	workspace                                                          *regionWorkspace
	children                                                           []*par2Transform
	inputBank                                                          [][]byte
}

func newPAR2Transform(available, missing, exponents []int, workers int) *par2Transform {
	b := par2AdditiveBasis()
	start, _ := consecutiveStart(exponents)
	k := len(missing)
	m := 1 << bits.Len(uint(2*k-1))
	q := 1 << bits.Len(uint(k-1))
	_, values, derivative := locatorValues(missing)
	p := &par2Transform{available: append([]int(nil), available...), missing: append([]int(nil), missing...), workers: max(1, workers)}
	p.weights = make([]gf2p16.T, len(available))
	p.scales = make([]gf2p16.T, k)
	for i, row := range available {
		y := generators[row]
		p.weights[i] = y.Pow(uint32(start))
	}
	for i, row := range missing {
		x := generators[row]
		p.scales[i] = derivative[x].Times(x.Pow(uint32(start))).Inverse()
	}
	p.degree = q
	p.toMoments = serialRegionProgram(transposeRegionProgram(b.conversionProgram(q, false)))
	for i := 0; i < q; i++ {
		p.sumMoments = addRegion(p.sumMoments, q+i, i, 1)
	}
	for base := 0; base < par2FieldSize; base += q {
		known, wanted := make([]bool, q), make([]bool, q)
		var survivor, result transformCoset
		for j, row := range available {
			point := int(b.indices[generators[row]])
			if point >= base && point < base+q {
				known[point-base] = true
				survivor.rows = append(survivor.rows, cosetRow{j, point - base})
				survivor.scales = scaleRegion(survivor.scales, point-base, p.weights[j])
			}
		}
		for j, row := range missing {
			point := int(b.indices[generators[row]])
			if point >= base && point < base+q {
				wanted[point-base] = true
				result.rows = append(result.rows, cosetRow{j, point - base})
				result.scales = scaleRegion(result.scales, point-base, p.scales[j])
			}
		}
		if len(survivor.rows) > 0 {
			survivor.program = serialRegionProgram(b.prunedFFTCoset(q, bits.Len(uint(q))-1, true, true, known, base))
			p.survivors = append(p.survivors, survivor)
		}
		if len(result.rows) > 0 {
			result.program = serialRegionProgram(b.prunedFFTCoset(q, bits.Len(uint(q))-1, false, false, wanted, base))
			result.program[0][0] = append(result.program[0][0], result.scales...)
			p.results = append(p.results, result)
		}
	}
	p.toProduct = b.conversionProgram(m, false)
	p.fromProduct = b.conversionProgram(m, true)
	p.forwardProduct = b.fftProgram(m, false)
	p.inverseProduct = b.fftProgram(m, true)
	p.toQuotient = b.conversionProgram(q, false)
	for i := 0; i < m; i++ {
		p.multiply = scaleRegion(p.multiply, i, values[b.points[i]])
	}

	return p
}
func (p *par2Transform) fold(n int, next func(int, []byte) error, out [][]byte) error {
	if p.workers == 1 || n <= transformStripeSize {
		return p.foldRange(n, next, out)
	}
	if len(p.children) != p.workers {
		p.close()
		if p.workspace != nil {
			p.workspace.close()
			p.workspace = nil
		}
		p.children = make([]*par2Transform, p.workers)
		for i := range p.children {
			child := *p
			child.workers = 1
			child.workspace = nil
			child.children = nil
			child.inputBank = nil
			p.children[i] = &child
		}
	}
	count := len(p.available) + len(p.missing)
	if len(p.inputBank) == 0 || len(p.inputBank[0]) < n {
		p.inputBank = make([][]byte, count)
		bank := make([]byte, count*n)
		for i := range p.inputBank {
			p.inputBank[i] = bank[i*n : (i+1)*n]
		}
	}
	for i := range p.inputBank {
		clear(p.inputBank[i])
		if err := next(i, p.inputBank[i][:n]); err != nil {
			return err
		}
	}
	var wg sync.WaitGroup
	var nextOffset atomic.Int64
	errs := make([]error, len(p.children))
	for worker, child := range p.children {
		wg.Add(1)
		go func(worker int, child *par2Transform) {
			defer wg.Done()
			rows := make([][]byte, len(out))
			for {
				off := int(nextOffset.Add(transformStripeSize)) - transformStripeSize
				if off >= n {
					break
				}
				size := min(transformStripeSize, n-off)
				for j := range rows {
					rows[j] = out[j][off : off+size]
				}
				if err := child.foldRange(size, func(i int, dst []byte) error { copy(dst, p.inputBank[i][off:off+size]); return nil }, rows); err != nil {
					errs[worker] = err
					return
				}
			}
		}(worker, child)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
func (p *par2Transform) foldRange(n int, next func(int, []byte) error, out [][]byte) error {
	if p.workspace == nil || p.workspace.size < n {
		p.close()
		w, err := newRegionWorkspace(n, 1, 2*p.degree)
		if err != nil {
			return err
		}
		p.workspace = w
	}
	w := p.workspace
	k := len(p.missing)
	w.clear()
	for _, coset := range p.survivors {
		for _, row := range w.rows[:p.degree] {
			clear(row)
		}
		for _, entry := range coset.rows {
			clear(w.raw)
			if err := next(entry.index, w.raw[:n]); err != nil {
				return err
			}
			w.ctx.Prepare(w.rows[entry.point], w.raw[:n])
		}
		if err := w.set.Apply(w.ctx, coset.scales); err != nil {
			return err
		}
		if err := w.run(coset.program); err != nil {
			return err
		}
		if err := w.set.Apply(w.ctx, p.sumMoments); err != nil {
			return err
		}
	}
	for i := 0; i < p.degree; i++ {
		copy(w.rows[i], w.rows[p.degree+i])
	}
	if err := w.run(p.toMoments); err != nil {
		return err
	}
	for j := 0; j < k; j++ {
		w.ctx.Finish(w.rows[j], out[j])
	}
	w.clear()
	// Subtract the surviving data's ordinary power sums from recovery rows.
	// Reverse these residual moments to form the polynomial R.
	for j := 0; j < k; j++ {
		clear(w.raw)
		if err := next(len(p.available)+j, w.raw[:n]); err != nil {
			return err
		}
		for i := 0; i < n; i++ {
			w.raw[i] ^= out[j][i]
		}
		w.ctx.Prepare(w.rows[k-1-j], w.raw[:n])
	}
	if err := w.run(p.toProduct); err != nil {
		return err
	}
	if err := w.run(p.forwardProduct); err != nil {
		return err
	}
	if err := w.set.Apply(w.ctx, p.multiply); err != nil {
		return err
	}
	if err := w.run(p.inverseProduct); err != nil {
		return err
	}
	if err := w.run(p.fromProduct); err != nil {
		return err
	}
	// The upper k ordinary coefficients of P*R are Q. Lower destinations
	// cannot overwrite any source, so the shift is safe in ascending order.
	for i := 0; i < k; i++ {
		copy(w.rows[i], w.rows[k+i])
	}
	for i := k; i < len(w.rows); i++ {
		clear(w.rows[i])
	}
	if err := w.run(p.toQuotient); err != nil {
		return err
	}
	// Keep Q in the upper half and reuse the lower half for each coset.
	for i := 0; i < p.degree; i++ {
		copy(w.rows[p.degree+i], w.rows[i])
	}
	for _, coset := range p.results {
		for i := 0; i < p.degree; i++ {
			copy(w.rows[i], w.rows[p.degree+i])
		}
		if err := w.run(coset.program); err != nil {
			return err
		}
		for _, entry := range coset.rows {
			w.ctx.Finish(w.rows[entry.point], out[entry.index])
		}
	}
	return nil
}
func (p *par2Transform) close() {
	for _, child := range p.children {
		child.close()
	}
	p.children = nil
	p.inputBank = nil
	if p.workspace != nil {
		p.workspace.close()
		p.workspace = nil
	}
}
