package rsec16

import (
	"math/rand"
	"runtime"
	"testing"

	"github.com/javi11/gopar-turbo/gf16"
)

// benchReconstruct measures repair throughput: dataShards inputs of
// sliceSize bytes with `missing` of them deleted, reconstructed from parity.
func benchReconstruct(b *testing.B, dataShards, parityShards, missing, sliceSize int) {
	rng := rand.New(rand.NewSource(7))
	c, err := NewCoderPAR2Vandermonde(dataShards, parityShards, runtime.NumCPU())
	if err != nil {
		b.Fatal(err)
	}
	orig := make([][]byte, dataShards)
	for i := range orig {
		orig[i] = make([]byte, sliceSize)
		rng.Read(orig[i])
	}
	parity := c.GenerateParity(orig)
	b.SetBytes(int64(dataShards * sliceSize))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		data := make([][]byte, dataShards)
		for j := range data {
			if j < missing {
				data[j] = nil
			} else {
				data[j] = orig[j]
			}
		}
		b.StartTimer()
		if err := c.ReconstructData(data, parity); err != nil {
			b.Fatal(err)
		}
	}
	b.Logf("backend accelerated=%v", gf16.Accelerated())
}

func BenchmarkReconstruct_100x10_64K(b *testing.B)  { benchReconstruct(b, 100, 10, 10, 65536) }
func BenchmarkReconstruct_1000x50_64K(b *testing.B) { benchReconstruct(b, 1000, 50, 50, 65536) }
func BenchmarkReconstruct_100x10_1M(b *testing.B)   { benchReconstruct(b, 100, 10, 10, 1<<20) }

func BenchmarkGenerateParity_100x10_64K(b *testing.B) {
	rng := rand.New(rand.NewSource(8))
	c, err := NewCoderPAR2Vandermonde(100, 10, runtime.NumCPU())
	if err != nil {
		b.Fatal(err)
	}
	data := make([][]byte, 100)
	for i := range data {
		data[i] = make([]byte, 65536)
		rng.Read(data[i])
	}
	b.SetBytes(int64(100 * 65536))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.GenerateParity(data)
	}
}
