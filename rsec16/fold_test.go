package rsec16

import (
	"errors"
	"math/rand"
	"runtime"
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

// The parallel fold must agree with the matrix path at awkward shapes:
// batch boundaries, more workers than rows, single input, tiny slices.
func TestFoldInputsParallelMatchesApplyMatrix(t *testing.T) {
	rng := rand.New(rand.NewSource(31))
	for _, tc := range []struct {
		name                         string
		numInputs, numOut, sliceSize int
		numGoroutines                int
	}{
		{"one batch exactly", 16, 3, 4096, 4},
		{"batch remainder", 37, 5, 4096, 8},
		{"fewer inputs than batch", 3, 2, 4096, 4},
		{"more workers than rows", 20, 2, 4096, 16},
		{"single input single row", 1, 1, 64, 4},
		{"goroutines=1", 37, 5, 4096, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := make([][]byte, tc.numInputs)
			for i := range in {
				in[i] = make([]byte, tc.sliceSize)
				rng.Read(in[i])
			}
			m := gf2p16.NewMatrixFromFunction(tc.numOut, tc.numInputs,
				func(i, j int) gf2p16.T { return gf2p16.T(rng.Intn(65536)) })

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
			err := FoldInputsParallel(m, tc.numInputs, tc.sliceSize, func(j int, buf []byte) error {
				copy(buf, in[j])
				return nil
			}, got, tc.numGoroutines)
			require.NoError(t, err)
			require.Equal(t, want, got)
		})
	}
}

// next must be called exactly once per input, strictly in order, even with
// prefetch running ahead of the fold.
func TestFoldInputsCallsNextSequentially(t *testing.T) {
	numInputs, sliceSize := 40, 4096
	m := gf2p16.NewMatrixFromFunction(2, numInputs,
		func(i, j int) gf2p16.T { return gf2p16.T(i + j + 1) })
	out := [][]byte{make([]byte, sliceSize), make([]byte, sliceSize)}

	var calls []int
	err := FoldInputs(m, numInputs, sliceSize, func(j int, buf []byte) error {
		calls = append(calls, j) // single-goroutine contract: no lock needed
		return nil
	}, out, 8)
	require.NoError(t, err)
	require.Len(t, calls, numInputs)
	for j, got := range calls {
		require.Equal(t, j, got, "next call order")
	}
}

// An error from next mid-batch must abort promptly and be returned.
func TestFoldInputsPropagatesMidBatchError(t *testing.T) {
	m := gf2p16.NewMatrixFromFunction(2, 40, func(i, j int) gf2p16.T { return 1 })
	out := [][]byte{make([]byte, 4096), make([]byte, 4096)}
	err := FoldInputs(m, 40, 4096, func(j int, buf []byte) error {
		if j == 21 { // second batch, mid-batch
			return errShardRead
		}
		return nil
	}, out, 8)
	require.ErrorIs(t, err, errShardRead)
}

// Catch the real allocation regression: one large output must not stage
// 48 full input slices. Measure the fold itself, excluding caller buffers.
func TestFoldInputsParallelLargeSliceMemory(t *testing.T) {
	const size = 4 << 20
	m := gf2p16.NewMatrixFromFunction(1, 32, func(i, j int) gf2p16.T { return 1 })
	out := [][]byte{make([]byte, size)}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	err := FoldInputsParallel(m, 32, size, func(j int, buf []byte) error {
		buf[0] = byte(j + 1)
		return nil
	}, out, 4)
	runtime.ReadMemStats(&after)
	require.NoError(t, err)
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(64<<20))
	require.Equal(t, byte(32), out[0][0])
	require.Zero(t, out[0][size-1])
}

func TestFoldInputsParallelAdaptiveBatch(t *testing.T) {
	const size, count, outputs = 512 << 10, 17, 4
	in := make([][]byte, count)
	rng := rand.New(rand.NewSource(57))
	for j := range in {
		in[j] = make([]byte, size)
		rng.Read(in[j][:size-2*j]) // trailing padding and partial reads
	}
	m := gf2p16.NewMatrixFromFunction(outputs, count, func(i, j int) gf2p16.T { return gf2p16.T(1 + i*count + j) })
	got, want := make([][]byte, outputs), make([][]byte, outputs)
	for i := range got {
		got[i] = make([]byte, size)
		want[i] = make([]byte, size)
	}
	applyMatrixParallelData(m, in, want, 3)
	require.NoError(t, FoldInputsParallel(m, count, size, func(j int, buf []byte) error {
		copy(buf, in[j][:size-2*j])
		return nil
	}, got, 3))
	require.Equal(t, want, got)
	require.ErrorIs(t, FoldInputsParallel(m, count, size, func(j int, buf []byte) error {
		if j == 7 {
			return errShardRead
		}
		copy(buf, in[j])
		return nil
	}, got, 3), errShardRead)
}
