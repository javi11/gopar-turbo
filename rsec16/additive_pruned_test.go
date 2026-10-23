package rsec16

import (
	"encoding/binary"
	"github.com/javi11/gopar-turbo/gf16"
	"github.com/javi11/gopar-turbo/gf2p16"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestPrunedFFT(t *testing.T) {
	b := newAdditiveBasis()
	for _, n := range []int{32, 256, 1024} {
		for _, inverse := range []bool{false, true} {
			mask := make([]bool, n)
			values := make([]gf2p16.T, n)
			for i := range values {
				mask[i] = i%17 == 3 || i == n-1
				if !inverse || mask[i] {
					values[i] = gf2p16.T(i*57 + 13)
				}
			}
			ctx, err := gf16.NewContext(2)
			require.NoError(t, err)
			set, err := gf16.NewBufferWorkspace(ctx, n)
			require.NoError(t, err)
			raw := make([]byte, 2)
			for i, v := range values {
				binary.LittleEndian.PutUint16(raw, uint16(v))
				ctx.Prepare(set.Buffers()[i], raw)
			}
			b.fft(values, inverse)
			require.NoError(t, runRegionProgram(set, []*gf16.Context{ctx}, b.prunedFFT(n, inverse, mask)))
			for i, v := range values {
				if inverse || mask[i] {
					ctx.Finish(set.Buffers()[i], raw)
					require.Equal(t, uint16(v), binary.LittleEndian.Uint16(raw), "n=%d inverse=%v row=%d", n, inverse, i)
				}
			}
			set.Close()
			ctx.Close()
		}
	}
}
