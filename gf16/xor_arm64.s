// Derived from the algorithms of ParPar (https://github.com/animetosho/ParPar, CC0); see gf16/LICENSE.parpar.

#include "textflag.h"

// func xorBlocks(dst, src *byte, n int)
// dst[0:n] ^= src[0:n]; n is a non-zero multiple of 32. dst may equal src.
TEXT ·xorBlocks(SB), NOSPLIT, $0-24
	MOVD dst+0(FP), R0
	MOVD src+8(FP), R1
	MOVD n+16(FP), R2
	CMP  $64, R2
	BLT  tail

loop64:
	VLD1.P 64(R1), [V0.B16, V1.B16, V2.B16, V3.B16]
	VLD1   (R0), [V4.B16, V5.B16, V6.B16, V7.B16]
	VEOR   V0.B16, V4.B16, V4.B16
	VEOR   V1.B16, V5.B16, V5.B16
	VEOR   V2.B16, V6.B16, V6.B16
	VEOR   V3.B16, V7.B16, V7.B16
	VST1.P [V4.B16, V5.B16, V6.B16, V7.B16], 64(R0)
	SUB    $64, R2
	CMP    $64, R2
	BGE    loop64

tail:
	CMP    $32, R2
	BLT    done
	VLD1.P 32(R1), [V0.B16, V1.B16]
	VLD1   (R0), [V4.B16, V5.B16]
	VEOR   V0.B16, V4.B16, V4.B16
	VEOR   V1.B16, V5.B16, V5.B16
	VST1   [V4.B16, V5.B16], (R0)

done:
	RET
