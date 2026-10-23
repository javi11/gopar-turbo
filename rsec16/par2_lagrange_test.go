package rsec16

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestPAR2LagrangeMatchesElimination(t *testing.T) {
	for _, start := range []int{0, 3, 65533} {
		const n = 17
		missing := []int{16, 1, 5}
		var available []int
		for i := 0; i < n; i++ {
			if i != 16 && i != 1 && i != 5 {
				available = append(available, i)
			}
		}
		rows := []int{start, start + 1, start + 2}
		c, err := NewCoderPAR2VandermondeRows(n, rows, 2)
		require.NoError(t, err)
		want, err := makeReconstructionMatrix(n, available, missing, []int{0, 1, 2}, c.ParityMatrix())
		require.NoError(t, err)
		got, ok := par2LagrangeMatrix(n, available, missing, rows)
		require.True(t, ok)
		require.Equal(t, want, got)
	}
	_, ok := par2LagrangeMatrix(4, []int{0}, []int{1, 2, 3}, []int{0, 3, 7})
	require.False(t, ok)
}
func TestPAR2LagrangeAllMissing(t *testing.T) {
	rows := []int{17, 18, 19, 20, 21}
	missing := []int{4, 2, 0, 3, 1}
	c, err := NewCoderPAR2VandermondeRows(5, rows, 1)
	require.NoError(t, err)
	want, err := makeReconstructionMatrix(5, nil, missing, []int{0, 1, 2, 3, 4}, c.ParityMatrix())
	require.NoError(t, err)
	got, ok := par2LagrangeMatrix(5, nil, missing, rows)
	require.True(t, ok)
	require.Equal(t, want, got)
}
