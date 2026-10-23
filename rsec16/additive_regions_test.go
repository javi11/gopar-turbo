package rsec16

import (
	"encoding/binary"
	"fmt"
	"github.com/javi11/gopar-turbo/gf16"
	"github.com/javi11/gopar-turbo/gf2p16"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestAdditiveRegionsMatchScalar(t *testing.T) {
	for _, n := range []int{32, 512, 2048} {
		t.Run(fmt.Sprint(n), func(t *testing.T) { testAdditiveRegionsMatchScalar(t, n) })
	}
}
func testAdditiveRegionsMatchScalar(t *testing.T, n int) {
	const size = 130
	b := newAdditiveBasis()
	ctx, err := gf16.NewContext(size)
	require.NoError(t, err)
	defer ctx.Close()
	rows := make([][]byte, n)
	for i := range rows {
		raw := make([]byte, size)
		for j := 0; j < size; j += 2 {
			binary.LittleEndian.PutUint16(raw[j:], uint16(i*71+j*17+1))
		}
		rows[i] = ctx.NewBuffer()
		ctx.Prepare(rows[i], raw)
	}
	set, err := gf16.NewBufferSet(ctx, rows)
	require.NoError(t, err)
	defer set.Close()
	contexts := make([]*gf16.Context, 3)
	for i := range contexts {
		contexts[i], err = gf16.NewContext(size)
		require.NoError(t, err)
		defer contexts[i].Close()
	}
	stages := []struct {
		program regionProgram
		scalar  func([]gf2p16.T)
	}{
		{b.conversionProgram(n, false), b.toNovel},
		{b.fftProgram(n, false), func(v []gf2p16.T) { b.fft(v, false) }},
		{b.fftProgram(n, true), func(v []gf2p16.T) { b.fft(v, true) }},
		{b.derivativeProgram(n), b.differentiate},
		{b.conversionProgram(n, true), b.fromNovel},
	}
	expected := make([][]gf2p16.T, size/2)
	for j := range expected {
		expected[j] = make([]gf2p16.T, n)
		for i := range expected[j] {
			expected[j][i] = gf2p16.T(i*71 + 2*j*17 + 1)
		}
	}
	for _, stage := range stages {
		for _, v := range expected {
			stage.scalar(v)
		}
		require.NoError(t, runRegionProgram(set, contexts, stage.program))
		for i := range rows {
			scratch := ctx.NewBuffer()
			copy(scratch, rows[i])
			raw := make([]byte, size)
			ctx.Finish(scratch, raw)
			for j := range expected {
				require.Equal(t, uint16(expected[j][i]), binary.LittleEndian.Uint16(raw[2*j:]))
			}
		}
	}
}
