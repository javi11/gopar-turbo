package gf16

import (
	"math/bits"
	"math/rand"
	"testing"

	"github.com/javi11/gopar-turbo/gf16/internal/gfmodel"
	"github.com/javi11/gopar-turbo/gf2p16"
)

// The GFNI kernels cannot run on every host, but the matrices they use are
// built in plain Go. These tests check that construction against a software
// model of GF2P8AFFINEQB (imm8 = 0, per the Intel SDM:
// result.bit[i] = parity(matrix.byte[7-i] & x)) and the Affine2x layout.

func affineQword(matrix uint64, x byte) byte {
	var r byte
	for i := 0; i < 8; i++ {
		row := byte(matrix >> (8 * (7 - i)))
		r |= byte(bits.OnesCount8(row&x)&1) << i
	}
	return r
}

func TestTranspose8(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for n := 0; n < 1000; n++ {
		x := rng.Uint64()
		var want uint64
		for r := 0; r < 8; r++ {
			for c := 0; c < 8; c++ {
				want |= (x >> (8*r + c) & 1) << (8*c + r)
			}
		}
		if got := gfmodel.Transpose8(x); got != want {
			t.Fatalf("gfmodel.Transpose8(%#x) = %#x, want %#x", x, got, want)
		}
	}
}

func TestAffineMatrixModel(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	coeffs := []uint16{0, 1, 2, 0x100b, 0x8000, 0xffff}
	for i := 0; i < 200; i++ {
		coeffs = append(coeffs, uint16(rng.Intn(1<<16)))
	}
	for _, c := range coeffs {
		m := gfmodel.AffineMatrix(c)
		for n := 0; n < 100; n++ {
			w := uint16(rng.Intn(1 << 16))
			lo, hi := byte(w), byte(w>>8)
			// one 16-byte lane: qword 0 = low bytes, qword 1 = high bytes.
			// norm = [Mll*lo | Mhh*hi], swap = [Mhl*lo | Mlh*hi]; the swap
			// qwords are exchanged before the XOR.
			normLo, normHi := affineQword(m[0], lo), affineQword(m[1], hi)
			swapLo, swapHi := affineQword(m[2], lo), affineQword(m[3], hi)
			gotLo, gotHi := normLo^swapHi, normHi^swapLo
			got := uint16(gotLo) | uint16(gotHi)<<8
			var in, out [2]byte
			in[0], in[1] = lo, hi
			gf2p16.MulByteSliceLE(gf2p16.T(c), in[:], out[:])
			if want := uint16(out[0]) | uint16(out[1])<<8; got != want {
				t.Fatalf("c=%#x w=%#x: model %#x, reference %#x", c, w, got, want)
			}
		}
	}
}
