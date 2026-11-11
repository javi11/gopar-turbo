// Derived from the algorithms of ParPar (https://github.com/animetosho/ParPar, CC0); see gf16/LICENSE.parpar.

#include "textflag.h"

// The prepared layout of the arm64 kernels is planar per 32-byte block: the
// 16 low bytes of the block's 16 little-endian words, then their 16 high
// bytes. That is what the multiply kernels want in registers (D0, D1) and
// loads with one cheap LDP, instead of a de-interleaving LD2 per source block.

// func prepareBlocks(dst, src *byte, n int)
// Converts n bytes (a non-zero multiple of 32) from plain to planar layout.
TEXT ·prepareBlocks(SB), NOSPLIT, $0-24
	MOVD dst+0(FP), R0
	MOVD src+8(FP), R1
	MOVD n+16(FP), R2

loop:
	VLD2.P 32(R1), [V0.B16, V1.B16]
	FSTPQ.P (F0, F1), 32(R0)
	SUBS $32, R2, R2
	BHI loop
	RET

// func finishBlocks(dst, src *byte, n int)
// Converts n bytes (a non-zero multiple of 32) from planar to plain layout.
TEXT ·finishBlocks(SB), NOSPLIT, $0-24
	MOVD dst+0(FP), R0
	MOVD src+8(FP), R1
	MOVD n+16(FP), R2

loop:
	FLDPQ.P 32(R1), (F0, F1)
	VST2.P [V0.B16, V1.B16], 32(R0)
	SUBS $32, R2, R2
	BHI loop
	RET
