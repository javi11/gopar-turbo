package rsec16

import (
	"math/rand"
	"runtime"
	"testing"

	"github.com/javi11/gopar-turbo/gf2p16"
)

// benchFold* mirror the benchmark set's repair-missing workload, scaled down
// in input count to keep memory sane: sliceSize matches the real set, and
// outputs/inputs keep the same ratio (~210/1965).
const (
	benchFoldInputs    = 256
	benchFoldOutputs   = 28
	benchFoldSliceSize = 2380956
)

func benchFoldSetup(b *testing.B) ([][]byte, gf2p16.Matrix, [][]byte) {
	b.Helper()
	rng := rand.New(rand.NewSource(42))
	in := make([][]byte, benchFoldInputs)
	for i := range in {
		in[i] = make([]byte, benchFoldSliceSize)
		rng.Read(in[i])
	}
	m := gf2p16.NewMatrixFromFunction(benchFoldOutputs, benchFoldInputs,
		func(i, j int) gf2p16.T { return gf2p16.T(rng.Intn(65535) + 1) })
	out := make([][]byte, benchFoldOutputs)
	for i := range out {
		out[i] = make([]byte, benchFoldSliceSize)
	}
	return in, m, out
}

// SetBytes reports GF work (inputs x sliceSize x outputs per op), so the
// reported MB/s is directly comparable to the aggregate throughput that
// BenchmarkReconstruct_1000x50_64K proves the matrix path can do.
func BenchmarkFoldInputs_Realistic(b *testing.B) {
	in, m, out := benchFoldSetup(b)
	b.SetBytes(int64(benchFoldInputs) * benchFoldSliceSize * int64(benchFoldOutputs))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		err := FoldInputs(m, benchFoldInputs, benchFoldSliceSize, func(j int, buf []byte) error {
			copy(buf, in[j])
			return nil
		}, out, runtime.NumCPU())
		if err != nil {
			b.Fatal(err)
		}
	}
}

// The reference: the existing all-in-memory parallel path on the same shape.
func BenchmarkFoldInputs_ApplyMatrixReference(b *testing.B) {
	in, m, out := benchFoldSetup(b)
	c := Coder{numGoroutines: runtime.NumCPU()}
	b.SetBytes(int64(benchFoldInputs) * benchFoldSliceSize * int64(benchFoldOutputs))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.applyMatrix(m, in, out)
	}
}
