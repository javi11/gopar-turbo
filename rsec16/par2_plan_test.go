package rsec16

import (
	"encoding/binary"
	"errors"
	"testing"

	"github.com/javi11/gopar-turbo/gf16"
	"github.com/stretchr/testify/require"
)

func TestPAR2PlanSelectionAndLifecycle(t *testing.T) {
	n, k := 8192, 1024
	missing, available, exponents := make([]int, k), make([]int, n-k), make([]int, k)
	for i := range missing {
		missing[i] = i
		exponents[i] = i
	}
	for i := range available {
		available[i] = k + i
	}
	for _, tc := range []struct {
		name      string
		budget    int
		transform bool
	}{{"default", 0, true}, {"limited", 32 << 20, false}, {"minimum transform", 64 << 20, true}} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := NewPAR2ReconstructionPlan(n, available, missing, exponents, 4, tc.budget)
			require.NoError(t, err)
			require.Equal(t, tc.transform, p.transform != nil)
			chunk, err := p.ChunkSize(1 << 20)
			require.NoError(t, err)
			require.Positive(t, chunk)
			require.Zero(t, chunk%2)
			if p.transform != nil {
				ctx, err := gf16.NewContext(chunk)
				require.NoError(t, err)
				bytes := int64(2*p.transform.degree+3)*int64(ctx.BufSize()) + int64(k)*int64(chunk)
				if p.transform.workers > 1 && chunk > transformStripeSize {
					stripeContext, err := gf16.NewContext(transformStripeSize)
					require.NoError(t, err)
					bytes = int64(p.transform.workers*(2*p.transform.degree+3)*stripeContext.BufSize()) + int64(n+k)*int64(chunk)
					stripeContext.Close()
				}
				ctx.Close()
				budget := tc.budget
				if budget == 0 {
					budget = 512 << 20
				}
				require.LessOrEqual(t, bytes, int64(budget))
				if p.transform.workers > 1 && chunk >= 4096 && chunk < 1<<20 {
					require.Zero(t, chunk%4096)
				}
			}
			if p.transform != nil {
				_, err = p.ChunkSize(int(^uint(0)>>1) &^ 1)
				require.NoError(t, err)
				require.Positive(t, p.transform.workers)
			}
			p.Close()
			p.Close()
			_, err = p.ChunkSize(64)
			require.Error(t, err)
			require.Error(t, p.Fold(2, func(int, []byte) error { return nil }, nil))
		})
	}
	// Gapped recovery exponents keep the general matrix solver.
	p, err := NewPAR2ReconstructionPlan(4, []int{0, 3}, []int{2, 1}, []int{0, 2}, 2, 0)
	require.NoError(t, err)
	defer p.Close()
	require.Nil(t, p.transform)
	for _, args := range []struct {
		n       int
		a, m, e []int
	}{{0, nil, nil, nil}, {3, []int{0}, []int{0, 2}, []int{0, 1}}, {3, []int{0, 1}, []int{3}, []int{0}}, {2, []int{0}, []int{1}, []int{65536}}, {2, []int{0}, []int{1}, nil}} {
		_, err := NewPAR2ReconstructionPlan(args.n, args.a, args.m, args.e, 2, 0)
		require.Error(t, err)
	}
}

func TestPAR2PlanDeepExactRecovery(t *testing.T) {
	t.Run("sparse damage", func(t *testing.T) { testPAR2PlanDeepExactRecovery(t, 8192, 1025) })
	t.Run("majority missing", func(t *testing.T) { testPAR2PlanDeepExactRecovery(t, 4096, 3073) })
}
func testPAR2PlanDeepExactRecovery(t *testing.T, n, k int) {
	// A non-power-of-two depth exercises high-half product extraction, an
	// offset exponent run and the production transform-selection threshold.
	const size = 4
	exponents := make([]int, k)
	missing := make([]int, k)
	present := make([]bool, n)
	for i := range exponents {
		exponents[i] = 13 + i
		if k <= n/2 {
			missing[i] = 2*(k-1-i) + 1
		} else {
			missing[i] = k - 1 - i
		}
		present[missing[i]] = true
	}
	var available []int
	for i := n - 1; i >= 0; i-- {
		if !present[i] {
			available = append(available, i)
		}
	}
	data := make([][]byte, n)
	for i := range data {
		data[i] = make([]byte, size)
		binary.LittleEndian.PutUint16(data[i], uint16(i*73+19))
		binary.LittleEndian.PutUint16(data[i][2:], uint16(i*17+91))
	}
	c, err := NewCoderPAR2VandermondeRows(n, exponents, 4)
	require.NoError(t, err)
	parity := make([][]byte, k)
	for i := range parity {
		parity[i] = make([]byte, size)
	}
	applyMatrixParallelData(c.ParityMatrix(), data, parity, 4) // independent ordinary multiplication
	var inputs [][]byte
	for _, row := range available {
		inputs = append(inputs, data[row])
	}
	inputs = append(inputs, parity...)
	p, err := NewPAR2ReconstructionPlan(n, available, missing, exponents, 4, 64<<20)
	require.NoError(t, err)
	defer p.Close()
	require.NotNil(t, p.transform)
	for _, chunk := range []int{size, 2} {
		out := make([][]byte, k)
		for i := range out {
			out[i] = make([]byte, chunk)
		}
		calls := make([]int, n)
		require.NoError(t, p.Fold(chunk, func(j int, dst []byte) error { calls[j]++; copy(dst, inputs[j][:chunk]); return nil }, out))
		for i, row := range missing {
			require.Equal(t, data[row][:chunk], out[i])
		}
		for _, count := range calls {
			require.Equal(t, 1, count)
		}
	}
	sentinel := errors.New("late parity read failure")
	out := make([][]byte, k)
	for i := range out {
		out[i] = make([]byte, 2)
	}
	require.ErrorIs(t, p.Fold(2, func(j int, dst []byte) error {
		if j == len(available)+17 {
			return sentinel
		}
		copy(dst, inputs[j][:2])
		return nil
	}, out), sentinel)
}
