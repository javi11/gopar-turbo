package rsec16

import (
	"bytes"
	"math/rand"
	"testing"

	"github.com/javi11/gopar-turbo/gf2p16"
)

func TestApplyMatrixGF16MatchesReference(t *testing.T) {
	rng := rand.New(rand.NewSource(6))
	for _, tc := range []struct {
		name                      string
		inShards, outShards, size int
		goroutines                int
	}{
		{"small", 3, 2, 8, 1},
		{"typical", 13, 5, 4096, 4},
		{"odd-slice", 7, 3, 4096 + 2, 4},
		{"more-goroutines-than-rows", 5, 2, 65536, 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := gf2p16.NewMatrixFromFunction(tc.outShards, tc.inShards,
				func(i, j int) gf2p16.T { return gf2p16.T(rng.Intn(1 << 16)) })
			in := make([][]byte, tc.inShards)
			for i := range in {
				in[i] = make([]byte, tc.size)
				rng.Read(in[i])
			}
			want := make([][]byte, tc.outShards)
			got := make([][]byte, tc.outShards)
			for i := range want {
				want[i] = make([]byte, tc.size)
				got[i] = make([]byte, tc.size)
			}
			applyMatrixParallelData(m, in, want, tc.goroutines)
			applyMatrixGF16(m, in, got, tc.goroutines)
			for i := range want {
				if !bytes.Equal(want[i], got[i]) {
					t.Errorf("output shard %d differs", i)
				}
			}
		})
	}
}
