// Package gfmodel holds slow, obviously correct Go models of the tables and
// matrices the amd64 GF(2^16) kernels build in assembly (or, for the
// GFNI kernels, in Go). The shuffle table model is used only from tests; the
// amd64 build links the package for the GF2P8AFFINEQB matrices.
package gfmodel

import "math/bits"

// Poly is ParPar's (and PAR2's) field polynomial x^16+x^12+x^3+x+1 without
// the x^16 term.
const Poly = 0x100b

// MulX returns v*x in GF(2^16).
func MulX(v uint16) uint16 {
	r := v << 1
	if v&0x8000 != 0 {
		r ^= Poly
	}
	return r
}

// Basis returns c*x^i for i in [0, 16).
func Basis(c uint16) (b [16]uint16) {
	for i := range b {
		b[i] = c
		c = MulX(c)
	}
	return b
}

// ShuffleTable holds the PSHUFB tables of the Shuffle kernels for one
// coefficient: for each nibble position k of a word (bits 4k..4k+3) a 16-byte
// table of the low byte and one of the high byte of coeff*(nibble<<4k), in
// the order low0 high0 low1 high1 low2 high2 low3 high3.
type ShuffleTable [128]byte

// NewShuffleTable builds the tables for coefficient c.
func NewShuffleTable(c uint16) (t ShuffleTable) {
	b := Basis(c)
	for k := 0; k < 4; k++ {
		var prod [16]uint16
		for n := 1; n < 16; n++ {
			prod[n] = prod[n&(n-1)] ^ b[4*k+bits.TrailingZeros(uint(n))]
		}
		lo, hi := t[32*k:32*k+16], t[32*k+16:32*k+32]
		for n, p := range prod {
			lo[n], hi[n] = byte(p), byte(p>>8)
		}
	}
	return t
}

// AffMat is the four 8x8 matrices [Mll, Mhh, Mhl, Mlh] of one coefficient as
// the 64-bit operands GF2P8AFFINEQB takes (the kernel broadcasts the first two
// as a 128-bit "norm" lane and the last two as the "swap" lane).
type AffMat [4]uint64

// Transpose8 transposes an 8x8 bit matrix stored as 8 bytes (bit 8r+c is
// element (r, c)); the classic three-step delta swap.
func Transpose8(x uint64) uint64 {
	const k1, k2, k4 = 0x5500550055005500, 0x3333000033330000, 0x0f0f0f0f00000000
	t := k4 & (x ^ x<<28)
	x ^= t ^ t>>28
	t = k2 & (x ^ x<<14)
	x ^= t ^ t>>14
	t = k1 & (x ^ x<<7)
	x ^= t ^ t>>7
	return x
}

// AffineMatrix builds the GF2P8AFFINEQB operands for multiplication by c.
//
// The instruction computes, for every data byte x and each result bit i,
// parity(A.byte[7-i] & x) (imm8 = 0): byte 7-i of the 64-bit operand holds
// the row of the matrix that produces output bit i, with bit j of that row
// selecting input bit j. The columns of the block for (out, in) are the
// images c*x^(8*in+j), j in [0,8), taken at byte 'out'; transposing gives the
// rows and reversing the bytes puts row i at byte 7-i.
func AffineMatrix(c uint16) AffMat {
	b := Basis(c)
	block := func(out, in int) uint64 {
		var cols uint64
		for j := 0; j < 8; j++ {
			cols |= uint64(byte(b[8*in+j]>>(8*out))) << (8 * j)
		}
		return bits.ReverseBytes64(Transpose8(cols))
	}
	return AffMat{block(0, 0), block(1, 1), block(1, 0), block(0, 1)}
}
