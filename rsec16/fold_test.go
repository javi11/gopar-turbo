package rsec16

import (
	"errors"
	"math/rand"
	"testing"

	"github.com/javi11/gopar-turbo/gf2p16"
	"github.com/stretchr/testify/require"
)

// FoldInputs must agree exactly with the all-in-memory matrix application,
// which is the behaviour every existing caller depends on.
func TestFoldInputsMatchesApplyMatrix(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	for _, tc := range []struct {
		name                         string
		numInputs, numOut, sliceSize int
	}{
		{"small", 4, 2, 64},
		{"medium", 9, 3, 256},
		{"wide", 17, 5, 1024},
		{"single output", 6, 1, 128},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := make([][]byte, tc.numInputs)
			for i := range in {
				in[i] = make([]byte, tc.sliceSize)
				rng.Read(in[i])
			}
			m := gf2p16.NewMatrixFromFunction(tc.numOut, tc.numInputs, func(i, j int) gf2p16.T {
				return gf2p16.T(rng.Intn(65536))
			})

			want := make([][]byte, tc.numOut)
			for i := range want {
				want[i] = make([]byte, tc.sliceSize)
			}
			c := Coder{numGoroutines: 4}
			c.applyMatrix(m, in, want)

			got := make([][]byte, tc.numOut)
			for i := range got {
				got[i] = make([]byte, tc.sliceSize)
			}
			err := FoldInputs(m, tc.numInputs, tc.sliceSize, func(j int, buf []byte) error {
				copy(buf, in[j])
				return nil
			}, got, 4)
			require.NoError(t, err)
			require.Equal(t, want, got)
		})
	}
}

var errShardRead = errors.New("shard read failed")

func TestFoldInputsPropagatesReadError(t *testing.T) {
	m := gf2p16.NewMatrixFromFunction(1, 2, func(i, j int) gf2p16.T { return 1 })
	out := [][]byte{make([]byte, 64)}
	err := FoldInputs(m, 2, 64, func(j int, buf []byte) error {
		if j == 1 {
			return errShardRead
		}
		return nil
	}, out, 1)
	require.ErrorIs(t, err, errShardRead)
}

// The supplied buffer is reused between calls, so a source that writes fewer
// bytes than a full slice must not see the previous input's tail.
func TestFoldInputsClearsBufferBetweenInputs(t *testing.T) {
	sliceSize := 32
	m := gf2p16.NewMatrixFromFunction(1, 2, func(i, j int) gf2p16.T { return 1 })

	got := [][]byte{make([]byte, sliceSize)}
	require.NoError(t, FoldInputs(m, 2, sliceSize, func(j int, buf []byte) error {
		if j == 0 {
			for i := range buf {
				buf[i] = 0xFF
			}
		}
		// j == 1 writes nothing: it must be treated as an all-zero shard.
		return nil
	}, got, 1))

	want := [][]byte{make([]byte, sliceSize)}
	in := [][]byte{make([]byte, sliceSize), make([]byte, sliceSize)}
	for i := range in[0] {
		in[0][i] = 0xFF
	}
	c := Coder{numGoroutines: 1}
	c.applyMatrix(m, in, want)
	require.Equal(t, want, got)
}

// applyMatrixParallelData does the same arithmetic without the SIMD backend,
// so comparing against it holds both a cgo and a non-cgo build to the same
// bytes. Without this, TestFoldInputsMatchesApplyMatrix would only ever
// compare a backend against itself.
func TestFoldInputsMatchesPureGoAcrossBackends(t *testing.T) {
	rng := rand.New(rand.NewSource(23))
	numInputs, numOut, sliceSize := 12, 4, 512

	in := make([][]byte, numInputs)
	for i := range in {
		in[i] = make([]byte, sliceSize)
		rng.Read(in[i])
	}
	m := gf2p16.NewMatrixFromFunction(numOut, numInputs, func(i, j int) gf2p16.T {
		return gf2p16.T(rng.Intn(65536))
	})

	want := make([][]byte, numOut)
	for i := range want {
		want[i] = make([]byte, sliceSize)
	}
	applyMatrixParallelData(m, in, want, 4)

	got := make([][]byte, numOut)
	for i := range got {
		got[i] = make([]byte, sliceSize)
	}
	require.NoError(t, FoldInputs(m, numInputs, sliceSize, func(j int, buf []byte) error {
		copy(buf, in[j])
		return nil
	}, got, 4))

	require.Equal(t, want, got)
}
