package rsec16

import (
	"errors"
	"math/rand"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPAR2TransformRestoresData(t *testing.T) {
	for _, tc := range []struct{ n, k, start int }{{17, 3, 0}, {17, 3, 7}, {4, 4, 65532}, {129, 32, 3}, {17, 13, 7}, {129, 97, 3}, {257, 193, 11}} {
		rng := rand.New(rand.NewSource(97))
		const size = 514
		data := make([][]byte, tc.n)
		for i := range data {
			data[i] = make([]byte, size)
			rng.Read(data[i])
		}
		exps := make([]int, tc.k)
		for i := range exps {
			exps[i] = tc.start + i
		}
		c, err := NewCoderPAR2VandermondeRows(tc.n, exps, 3)
		require.NoError(t, err)
		parity := c.GenerateParity(data)
		missing := make([]int, tc.k)
		for i := range missing {
			missing[i] = tc.k - 1 - i
		}
		var available []int
		var inputs [][]byte
		for i := tc.k; i < tc.n; i++ {
			available = append(available, i)
			inputs = append(inputs, data[i])
		}
		inputs = append(inputs, parity...)
		plan := newPAR2Transform(available, missing, exps, 3)
		// Reuse the workspace with a shorter final range as disk repair does.
		for _, n := range []int{2, size, 258, 2} {
			out := make([][]byte, tc.k)
			for i := range out {
				out[i] = make([]byte, n)
			}
			require.NoError(t, plan.fold(n, func(j int, dst []byte) error { copy(dst, inputs[j][:n]); return nil }, out))
			for i, row := range missing {
				require.Equal(t, data[row][:n], out[i], "n=%d k=%d start=%d", tc.n, tc.k, tc.start)
			}
		}
		sentinel := errors.New("input read failed")
		out := make([][]byte, tc.k)
		for i := range out {
			out[i] = make([]byte, 2)
		}
		require.ErrorIs(t, plan.fold(2, func(j int, dst []byte) error { return sentinel }, out), sentinel)
		plan.close()
	}
}
