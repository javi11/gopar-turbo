//go:build ignore

// x86 emits the amd64 GF(2^16) region kernels:
//
//	shuffle_amd64.s  Shuffle (SSSE3) and Shuffle (AVX2)
//	affine_amd64.s   Affine2x (GFNI+AVX2) and Affine2x (GFNI+AVX-512);
//	                 chosen at run time only on CPUs with GFNI
//
// Derived from ParPar (CC0); see gf16/LICENSE.parpar.
//
// Run it with `go generate ./gf16` or `go run asmgen/x86.go` from the gf16
// directory. The kernels port ParPar's gf16_shuffle_x86.h and
// gf16_affine2x_x86.h; see x86_amd64.go and affine_unverified_amd64.go for the
// algorithms, the prepared layouts and the Go-side table builders.
//
// # Conventions
//
// The generator writes three-operand "Intel semantics" (dst = op(a, b)) and
// this file lowers them: SSE gets MOVO copies for the destructive two operand
// forms, VEX/EVEX forms get Go's reversed operand order. Every kernel keeps
// end-of-region pointers and a negative byte offset in CX that counts up to
// zero, the way ParPar's loops do, so one ADD both advances and terminates.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/javi11/gopar-turbo/gf16/internal/gfmodel"
)

type isa int

const (
	sse isa = iota
	avx2
	avx512
	avx128 // VEX encoding on 128-bit registers (table setup of the AVX2 kernels)
)

func (i isa) reg() string { return [...]string{"X", "Y", "Z", "X"}[i] }
func (i isa) vec() int    { return [...]int{16, 32, 64, 16}[i] }
func (i isa) vex() bool   { return i != sse }

type gen struct {
	sb        strings.Builder
	k         isa
	surrogate bool // affine: emit XOR instead of GF2P8AFFINEQB (see surrogateMarker)
}

func (g *gen) p(f string, a ...any) { fmt.Fprintf(&g.sb, f+"\n", a...) }
func (g *gen) ins(f string, a ...any) {
	g.sb.WriteByte('\t')
	fmt.Fprintf(&g.sb, f, a...)
	g.sb.WriteByte('\n')
}

// vexIdx selects the VEX (0) or EVEX (1) spelling of an instruction.
func (g *gen) vexIdx() int {
	if g.k == avx512 {
		return 1
	}
	return 0
}

func (g *gen) r(i int) string { return fmt.Sprintf("%s%d", g.k.reg(), i) }

// load/store: unaligned full-vector moves.
func (g *gen) load(d int, mem string) {
	g.ins("%s %s, %s", [...]string{"MOVOU", "VMOVDQU", "VMOVDQU64", "VMOVDQU"}[g.k], mem, g.r(d))
}
func (g *gen) store(s int, mem string) {
	g.ins("%s %s, %s", [...]string{"MOVOU", "VMOVDQU", "VMOVDQU64", "VMOVDQU"}[g.k], g.r(s), mem)
}

// bcast loads 16 bytes at mem into every 128-bit lane of d.
func (g *gen) bcast(d int, mem string) {
	g.ins("%s %s, %s", [...]string{"MOVOU", "VBROADCASTI128", "VBROADCASTI32X4", "VMOVDQU"}[g.k], mem, g.r(d))
}

func (g *gen) mov(d, s int) {
	if d == s {
		return
	}
	g.ins("%s %s, %s", [...]string{"MOVO", "VMOVDQA", "VMOVDQA64", "VMOVDQA"}[g.k], g.r(s), g.r(d))
}

// bin emits d = a op b for the commutative ops "and" and "xor".
func (g *gen) bin(op string, d, a, b int) {
	if g.k == sse {
		name := map[string]string{"and": "PAND", "xor": "PXOR"}[op]
		switch {
		case d == a:
			g.ins("%s %s, %s", name, g.r(b), g.r(d))
		case d == b:
			g.ins("%s %s, %s", name, g.r(a), g.r(d))
		default:
			g.mov(d, a)
			g.ins("%s %s, %s", name, g.r(b), g.r(d))
		}
		return
	}
	name := map[string][2]string{"and": {"VPAND", "VPANDQ"}, "xor": {"VPXOR", "VPXORQ"}}[op][g.vexIdx()]
	g.ins("%s %s, %s, %s", name, g.r(b), g.r(a), g.r(d))
}

// xorMem emits d = a ^ mem (mem unaligned-safe only for VEX/EVEX forms).
func (g *gen) xorMem(d, a int, mem string) {
	if g.k == sse {
		panic("xorMem on SSE")
	}
	name := [...]string{"", "VPXOR", "VPXORQ", "VPXOR"}[g.k]
	g.ins("%s %s, %s, %s", name, mem, g.r(a), g.r(d))
}

// srl4 emits d = (s >> 4) per 16-bit word (callers mask afterwards).
func (g *gen) srl4(d, s int) {
	if g.k == sse {
		g.mov(d, s)
		g.ins("PSRLW $4, %s", g.r(d))
		return
	}
	g.ins("VPSRLW $4, %s, %s", g.r(s), g.r(d))
}

// shl4 emits d = s << 4 per 16-bit word (callers mask beforehand).
func (g *gen) shl4(d, s int) {
	if g.k == sse {
		g.mov(d, s)
		g.ins("PSLLW $4, %s", g.r(d))
		return
	}
	g.ins("VPSLLW $4, %s, %s", g.r(s), g.r(d))
}

// shufImm emits d = PSHUFLW/PSHUFD imm(s) (SSE2 forms are non-destructive).
func (g *gen) shufImm(op string, imm int, d, s int) {
	if g.k.vex() {
		op = "V" + op
	}
	g.ins("%s $%#x, %s, %s", op, imm, g.r(s), g.r(d))
}

// pshufb emits d[i] = tbl[idx[i] & 15] (within 128-bit lanes). d must differ
// from idx.
func (g *gen) pshufb(d, tbl, idx int) {
	if d == idx {
		panic("pshufb: dst aliases index")
	}
	if g.k == sse {
		g.mov(d, tbl)
		g.ins("PSHUFB %s, %s", g.r(idx), g.r(d))
		return
	}
	g.ins("VPSHUFB %s, %s, %s", g.r(idx), g.r(tbl), g.r(d))
}

// unpack emits the in-lane unpack of a (first) and b (second): kind is one of
// "lbw" "hbw" "lqdq" "hqdq".
func (g *gen) unpack(kind string, d, a, b int) {
	if g.k == sse {
		name := map[string]string{"lbw": "PUNPCKLBW", "hbw": "PUNPCKHBW", "lqdq": "PUNPCKLQDQ", "hqdq": "PUNPCKHQDQ"}[kind]
		if d == b && d != a {
			panic("unpack: dst aliases second operand")
		}
		g.mov(d, a)
		g.ins("%s %s, %s", name, g.r(b), g.r(d))
		return
	}
	name := map[string]string{"lbw": "VPUNPCKLBW", "hbw": "VPUNPCKHBW", "lqdq": "VPUNPCKLQDQ", "hqdq": "VPUNPCKHQDQ"}[kind]
	g.ins("%s %s, %s, %s", name, g.r(b), g.r(a), g.r(d))
}

func (g *gen) fn(name, frame string) {
	g.p("")
	g.p("TEXT ·%s(SB), NOSPLIT, $%s", name, frame)
}

func (g *gen) ret() {
	if g.k.vex() {
		g.ins("VZEROUPPER")
	}
	g.ins("RET")
}

// ---------------------------------------------------------------- shuffle

// Register plan of the shuffle kernels (a "block" is 2 vectors; the first
// holds the high bytes of the block's words, the second the low bytes):
//
//	0..7  tables low0 high0 low1 high1 low2 high2 low3 high3 (broadcast per lane)
//	8     mask 0x0f      9 ta (high bytes)   10 tb (low bytes)
//	11    nibble index   12 tpl              13 tph             14 scratch
//
// The tables are built inline from the coefficient argument (setupBody), so a
// call has no table memory traffic and no separate setup call.
//
// Nibble k of a word selects entry k of low_k/high_k: the low and high result
// byte of (coeff * nibble << 4k). tb carries nibbles 0 and 1, ta nibbles 2
// and 3, exactly as in ParPar's gf16_shuffle_muladd_round.
// tablesFromCoeff loads the 8 table registers (X0..X7, or Y0..Y7 with both
// lanes equal) for the coefficient in BX; see setupBody for what it clobbers.
func (g *gen) tablesFromCoeff() {
	if g.k == sse {
		g.setupBody()
		return
	}
	// Build the tables with VEX-128 code, then duplicate them into the upper
	// lane of each Y register.
	g.k = avx128
	g.setupBody()
	g.k = avx2
	for i := 0; i < 8; i++ {
		g.ins("VINSERTI128 $1, X%d, Y%d, Y%d", i, i, i)
	}
}

// shuffleLoop emits the block loop over n bytes (CX) from src (BX) to dst
// (AX) with the tables in registers 0..7; it falls through when done.
func (g *gen) shuffleLoop(add bool) {
	v := g.k.vec()
	g.bcast(8, "mask0f<>(SB)")
	g.ins("ADDQ CX, AX")
	g.ins("ADDQ CX, BX")
	g.ins("NEGQ CX")
	g.p("loop:")
	g.load(9, "(BX)(CX*1)")
	g.load(10, fmt.Sprintf("%d(BX)(CX*1)", v))
	g.bin("and", 11, 8, 10)
	g.pshufb(13, 1, 11) // tph
	g.pshufb(12, 0, 11) // tpl
	g.srl4(10, 10)
	g.bin("and", 10, 10, 8)
	g.pshufb(14, 3, 10)
	g.bin("xor", 13, 13, 14)
	g.pshufb(14, 2, 10)
	g.bin("xor", 12, 12, 14)
	if add {
		if g.k == sse {
			g.load(14, "(AX)(CX*1)")
			g.bin("xor", 13, 13, 14)
			g.load(14, fmt.Sprintf("%d(AX)(CX*1)", v))
			g.bin("xor", 12, 12, 14)
		} else {
			g.xorMem(13, 13, "(AX)(CX*1)")
			g.xorMem(12, 12, fmt.Sprintf("%d(AX)(CX*1)", v))
		}
	}
	g.bin("and", 11, 8, 9)
	g.pshufb(14, 5, 11)
	g.bin("xor", 13, 13, 14)
	g.pshufb(14, 4, 11)
	g.bin("xor", 12, 12, 14)
	g.srl4(9, 9)
	g.bin("and", 9, 9, 8)
	g.pshufb(14, 7, 9)
	g.bin("xor", 13, 13, 14)
	g.pshufb(14, 6, 9)
	g.bin("xor", 12, 12, 14)
	g.store(13, "(AX)(CX*1)")
	g.store(12, fmt.Sprintf("%d(AX)(CX*1)", v))
	g.ins("ADDQ $%d, CX", 2*v)
	g.ins("JNZ loop")
}

func (g *gen) shuffleKernel(name string, add bool) {
	g.fn(name, "0-26")
	g.ins("MOVWLZX coeff+24(FP), BX")
	g.tablesFromCoeff()
	g.ins("MOVQ dst+0(FP), AX")
	g.ins("MOVQ src+8(FP), BX")
	g.ins("MOVQ n+16(FP), CX")
	g.shuffleLoop(add)
	g.ret()
}

// shuffleList emits the multi-source driver: dst ^= sum coeffs[i]*srcs[i] as
// one pass per source, tables built inline per pass, in a single call (the
// Go to assembly call costs more than a whole 64-byte pass). Sources equal
// to dst and zero coefficients are skipped; the Go wrapper has already
// applied the aliasing sources of dst. R9 srcs cursor, R10 coeffs cursor,
// R11 sources left, R12 dst, R13 n.
func (g *gen) shuffleList(name string) {
	g.fn(name, "0-40")
	g.ins("MOVQ dst+0(FP), R12")
	g.ins("MOVQ srcs+8(FP), R9")
	g.ins("MOVQ coeffs+16(FP), R10")
	g.ins("MOVQ nsrc+24(FP), R11")
	g.ins("MOVQ n+32(FP), R13")
	g.ins("TESTQ R11, R11")
	g.ins("JLE done")
	g.p("next:")
	g.ins("MOVWLZX (R10), BX")
	g.ins("TESTL BX, BX")
	g.ins("JEQ skip")
	g.ins("MOVQ (R9), AX")
	g.ins("CMPQ AX, R12")
	g.ins("JEQ skip")
	g.tablesFromCoeff()
	g.ins("MOVQ R12, AX")
	g.ins("MOVQ (R9), BX")
	g.ins("MOVQ R13, CX")
	g.shuffleLoop(true)
	g.p("skip:")
	g.ins("ADDQ $8, R9")
	g.ins("ADDQ $2, R10")
	g.ins("DECQ R11")
	g.ins("JNZ next")
	g.p("done:")
	g.ret()
}

// packBytes returns the little-endian qwords of bs (len a multiple of 8).
func packBytes(bs []byte) (q []uint64) {
	for i := 0; i < len(bs); i += 8 {
		var v uint64
		for j := 7; j >= 0; j-- {
			v = v<<8 | uint64(bs[i+j])
		}
		q = append(q, v)
	}
	return q
}

func (g *gen) data(name string, bs []byte) {
	for i, q := range packBytes(bs) {
		g.p("DATA %s<>+%d(SB)/8, $%#016x", name, 8*i, q)
	}
	g.p("GLOBL %s<>(SB), RODATA|NOPTR, $%d", name, len(bs))
}

// setupTables emits qtab, the constant behind setupBody: qtab[k][v] is the
// 128-byte shufTable of the coefficient v<<4k (k < 4, v < 16), taken from the
// Go model.
func (g *gen) setupTables() {
	var qtab []byte
	for k := 0; k < 4; k++ {
		for v := 0; v < 16; v++ {
			t := gfmodel.NewShuffleTable(uint16(v) << (4 * k))
			qtab = append(qtab, t[:]...)
		}
	}
	g.p("")
	g.p("// qtab[k][v] = the 128-byte nibble tables of coefficient v<<4k (k, v < 16).")
	g.data("qtab", qtab)
}

// Offsets inside qtab.
const (
	qtabRow    = 128      // bytes per table
	qtabNibble = 16 * 128 // bytes per nibble position
)

// setupBody emits the table builder (the role of ParPar's
// gf16_shuffle_setup_vec). On entry BX holds the coefficient c (zero
// extended); on exit X0..X7 hold the tables low0 high0 low1 high1 low2 high2
// low3 high3 (upper halves of the Y registers are zero). Clobbers BX CX DX SI
// DI R8 and X8..X14. g.k must be sse or avx128.
//
// Every table entry is c*(n<<4k), which is linear (XOR) in c. Split
// c = c0 ^ c1<<4 ^ c2<<8 ^ c3<<12 into nibbles: the tables of c are then the
// XOR of the four precomputed tables of (c_j<<4j), looked up in qtab. That is
// 4 independent 128-byte loads XORed per table, with no dependency chain,
// instead of ParPar's vector arithmetic (which is latency bound: about 40
// cycles end to end, as much as the whole kernel for a 64-byte region).
func (g *gen) setupBody() {
	g.ins("LEAQ qtab<>(SB), DI")
	rows := [4]string{"CX", "DX", "SI", "R8"}
	for j, r := range rows {
		// r = ((c >> 4j) & 0xf) * 128 = (c << 7 >> 4j) & 0x780
		g.ins("MOVL BX, %s", r)
		if s := 4*j - 7; s > 0 {
			g.ins("SHRL $%d, %s", s, r)
		} else if s < 0 {
			g.ins("SHLL $%d, %s", -s, r)
		}
		g.ins("ANDL $%#x, %s", 15*qtabRow, r)
		g.ins("ADDQ DI, %s", r)
	}
	for i := 0; i < 8; i++ {
		g.load(i, fmt.Sprintf("%d(CX)", 16*i))
		g.load(8, fmt.Sprintf("%d(DX)", qtabNibble+16*i))
		g.load(9, fmt.Sprintf("%d(SI)", 2*qtabNibble+16*i))
		g.load(10, fmt.Sprintf("%d(R8)", 3*qtabNibble+16*i))
		g.bin("xor", i, i, 8)
		g.bin("xor", 9, 9, 10)
		g.bin("xor", i, i, 9)
	}
}

// shuffleSetup emits func(t *shufTable, c uint16), storing setupBody's
// tables; it exists so tests can check the table builder directly (the
// kernels run the very same code inline).
func (g *gen) shuffleSetup(name string) {
	g.fn(name, "0-10")
	g.ins("MOVQ t+0(FP), AX")
	g.ins("MOVWLZX c+8(FP), BX")
	g.setupBody()
	for i := 0; i < 8; i++ {
		g.store(i, fmt.Sprintf("%d(AX)", 16*i))
	}
	g.ret()
}

// prepareBlocks converts blocks of 2 vectors from the standard word layout.
// Per 128-bit lane: separate low and high bytes (PSHUFB), then unpack the
// high halves of the two vectors into the first output and the low halves
// into the second, as ParPar's gf16_shuffle_prepare_block does.
func (g *gen) shufflePrepare(name string) {
	v := g.k.vec()
	g.fn(name, "0-24")
	g.ins("MOVQ dst+0(FP), AX")
	g.ins("MOVQ src+8(FP), BX")
	g.ins("MOVQ n+16(FP), CX")
	g.bcast(8, "sepmask<>(SB)")
	g.p("loop:")
	g.load(0, "(BX)")
	g.load(1, fmt.Sprintf("%d(BX)", v))
	g.pshufb(2, 0, 8)
	g.pshufb(3, 1, 8)
	g.unpack("hqdq", 4, 2, 3)
	g.unpack("lqdq", 5, 2, 3)
	g.store(4, "(AX)")
	g.store(5, fmt.Sprintf("%d(AX)", v))
	g.ins("ADDQ $%d, BX", 2*v)
	g.ins("ADDQ $%d, AX", 2*v)
	g.ins("SUBQ $%d, CX", 2*v)
	g.ins("JNZ loop")
	g.ret()
}

func (g *gen) shuffleFinish(name string) {
	v := g.k.vec()
	g.fn(name, "0-24")
	g.ins("MOVQ dst+0(FP), AX")
	g.ins("MOVQ src+8(FP), BX")
	g.ins("MOVQ n+16(FP), CX")
	g.p("loop:")
	g.load(0, "(BX)")                   // high bytes
	g.load(1, fmt.Sprintf("%d(BX)", v)) // low bytes
	g.unpack("lbw", 2, 1, 0)
	g.unpack("hbw", 3, 1, 0)
	g.store(2, "(AX)")
	g.store(3, fmt.Sprintf("%d(AX)", v))
	g.ins("ADDQ $%d, BX", 2*v)
	g.ins("ADDQ $%d, AX", 2*v)
	g.ins("SUBQ $%d, CX", 2*v)
	g.ins("JNZ loop")
	g.ret()
}

// xorKernel: dst ^= src, one vector per step, two steps per iteration. n is
// a multiple of 2 vectors for SSE and of 1 vector otherwise, so a trailing
// single step is emitted for the VEX forms.
func (g *gen) xorKernel(name string) {
	v := g.k.vec()
	g.fn(name, "0-24")
	g.ins("MOVQ dst+0(FP), AX")
	g.ins("MOVQ src+8(FP), BX")
	g.ins("MOVQ n+16(FP), CX")
	g.ins("CMPQ CX, $%d", 2*v)
	g.ins("JLT tail")
	g.p("loop:")
	for i := 0; i < 2; i++ {
		g.load(i, fmt.Sprintf("%d(BX)", i*v))
		if g.k == sse {
			g.load(2+i, fmt.Sprintf("%d(AX)", i*v))
			g.bin("xor", i, i, 2+i)
		} else {
			g.xorMem(i, i, fmt.Sprintf("%d(AX)", i*v))
		}
		g.store(i, fmt.Sprintf("%d(AX)", i*v))
	}
	g.ins("ADDQ $%d, AX", 2*v)
	g.ins("ADDQ $%d, BX", 2*v)
	g.ins("SUBQ $%d, CX", 2*v)
	g.ins("CMPQ CX, $%d", 2*v)
	g.ins("JGE loop")
	g.p("tail:")
	g.ins("TESTQ CX, CX")
	g.ins("JZ done")
	g.load(0, "(BX)")
	if g.k == sse {
		g.load(2, "(AX)")
		g.bin("xor", 0, 0, 2)
	} else {
		g.xorMem(0, 0, "(AX)")
	}
	g.store(0, "(AX)")
	g.p("done:")
	g.ret()
}

func shuffleFile() string {
	g := &gen{}
	g.p("// Code generated by asmgen/x86.go; DO NOT EDIT.")
	g.p("// Derived from the algorithms of ParPar (https://github.com/animetosho/ParPar, CC0); see gf16/LICENSE.parpar.")
	g.p("")
	g.p("#include \"textflag.h\"")
	g.p("")
	g.p("// Per-byte 0x0f nibble mask.")
	g.p("DATA mask0f<>+0(SB)/8, $0x0f0f0f0f0f0f0f0f")
	g.p("DATA mask0f<>+8(SB)/8, $0x0f0f0f0f0f0f0f0f")
	g.p("GLOBL mask0f<>(SB), RODATA|NOPTR, $16")
	g.p("")
	g.p("// PSHUFB control: even source bytes (low halves of words), then odd ones.")
	g.p("DATA sepmask<>+0(SB)/8, $0x0e0c0a0806040200")
	g.p("DATA sepmask<>+8(SB)/8, $0x0f0d0b0907050301")
	g.p("GLOBL sepmask<>(SB), RODATA|NOPTR, $16")
	for _, c := range []struct {
		k    isa
		name string
	}{{sse, "SSSE3"}, {avx2, "AVX2"}} {
		g.k = c.k
		g.shuffleKernel("shufMul"+c.name, false)
		g.shuffleKernel("shufMulAdd"+c.name, true)
		g.shuffleList("shufMulAddList" + c.name)
		g.shufflePrepare("shufPrepare" + c.name)
		g.shuffleFinish("shufFinish" + c.name)
	}
	g.setupTables()
	g.k = sse
	g.shuffleSetup("shufSetupSSSE3")
	g.k = avx128
	g.shuffleSetup("shufSetupAVX2")
	g.k = sse
	g.xorKernel("xorSSE")
	g.k = avx2
	g.xorKernel("xorAVX2")
	return g.sb.String()
}

// ----------------------------------------------------------------- affine

// affineMulti emits the Affine2x kernel. A prepared vector holds, per 128-bit
// lane, the low bytes of 8 words (qword 0) and their high bytes (qword 1).
// For each source two GF2P8AFFINEQB per vector produce the "norm" products
// (lo*Mll | hi*Mhh) and the "swap" products (lo*Mhl | hi*Mlh); the swap
// accumulator's qwords are exchanged once at the end and XORed in.
//
// mats (see affine_unverified_amd64.go) is 32 bytes per source: norm lane (16 bytes),
// swap lane (16 bytes).
func (g *gen) affineMulti(name string, regions int, add bool) {
	v := g.k.vec()
	srcRegs := []string{"BX", "SI", "DI", "R8", "R9", "R10", "R11", "DX"}
	// vector registers: 2 matrices per region, then data, r1, r2, tmp.
	data, r1, r2, tmp := 2*regions, 2*regions+1, 2*regions+2, 2*regions+3
	g.fn(name, "0-40")
	g.ins("MOVQ dst+0(FP), AX")
	g.ins("MOVQ srcs+8(FP), R12")
	g.ins("MOVQ mats+16(FP), R13")
	g.ins("MOVQ nsrc+24(FP), DX")
	g.ins("MOVQ n+32(FP), CX")
	counts := regions
	if !add {
		counts = 1
	}
	if counts > 1 {
		for k := 1; k < counts; k++ {
			g.ins("CMPQ DX, $%d", k)
			g.ins("JEQ body%d", k)
		}
	}
	for k := counts; k >= 1; k-- {
		g.p("body%d:", k)
		for r := 0; r < k; r++ {
			g.ins("MOVQ %d(R12), %s", 8*r, srcRegs[r])
			g.ins("ADDQ CX, %s", srcRegs[r])
			g.bcast(2*r, fmt.Sprintf("%d(R13)", 32*r))
			g.bcast(2*r+1, fmt.Sprintf("%d(R13)", 32*r+16))
		}
		g.ins("ADDQ CX, AX")
		g.ins("NEGQ CX")
		g.p("loop%d:", k)
		for r := 0; r < k; r++ {
			g.load(data, fmt.Sprintf("(%s)(CX*1)", srcRegs[r]))
			a1, a2 := tmp, tmp
			if r == 0 {
				a1, a2 = r1, r2
			}
			g.affine(2*r, data, a1)
			if r != 0 {
				g.bin("xor", r1, r1, a1)
			}
			g.affine(2*r+1, data, a2)
			if r != 0 {
				g.bin("xor", r2, r2, a2)
			}
		}
		g.ins("VPSHUFD $0x4e, %s, %s", g.r(r2), g.r(r2))
		g.bin("xor", r1, r1, r2)
		if add {
			g.xorMem(r1, r1, "(AX)(CX*1)")
		}
		g.store(r1, "(AX)(CX*1)")
		g.ins("ADDQ $%d, CX", v)
		g.ins("JNZ loop%d", k)
		g.ret()
	}
}

// affine emits d = GF2P8AFFINEQB(data, mat). In surrogate mode (see
// surrogateMarker) the instruction is replaced by a plain XOR.
func (g *gen) affine(mat, data, d int) {
	if g.surrogate {
		g.bin("xor", d, data, mat)
		return
	}
	g.ins("VGF2P8AFFINEQB $0, %s, %s, %s", g.r(mat), g.r(data), g.r(d))
}

func (g *gen) affinePrepare(name string, finish bool) {
	g.fn(name, "0-24")
	g.ins("MOVQ dst+0(FP), AX")
	g.ins("MOVQ src+8(FP), BX")
	g.ins("MOVQ n+16(FP), CX")
	if finish {
		g.bcast(1, "finmask<>(SB)")
	} else {
		g.bcast(1, "sepmask<>(SB)")
	}
	g.p("loop:")
	g.load(0, "(BX)")
	g.pshufb(2, 0, 1)
	g.store(2, "(AX)")
	g.ins("ADDQ $%d, BX", g.k.vec())
	g.ins("ADDQ $%d, AX", g.k.vec())
	g.ins("SUBQ $%d, CX", g.k.vec())
	g.ins("JNZ loop")
	g.ret()
}

// surrogateMarker is stamped on every surrogate file. Surrogate output
// replaces GF2P8AFFINEQB by XOR, a development aid that lets the affine
// kernels' addressing, dispatch and accumulation be executed (and compared
// with a Go model) on machines without GFNI. Its products are WRONG, so it is
// only ever written to an explicitly named other file (-surrogate), never to
// affine_amd64.s, and gf16's TestCommittedAffineAssembly fails if the
// marker appears in the committed file.
const surrogateMarker = "GF16_SURROGATE_OUTPUT"

func affineFile(surrogate bool) string {
	g := &gen{surrogate: surrogate}
	if surrogate {
		g.p("// %s: NOT REAL OUTPUT. GF2P8AFFINEQB IS REPLACED BY XOR, SO EVERY PRODUCT IS WRONG.", surrogateMarker)
		g.p("// %s: development aid only; NEVER commit, build into a release or copy over affine_amd64.s.", surrogateMarker)
	}
	g.p("// Code generated by asmgen/x86.go; DO NOT EDIT.")
	g.p("// Derived from the algorithms of ParPar (https://github.com/animetosho/ParPar, CC0); see gf16/LICENSE.parpar.")
	g.p("")
	g.p("#include \"textflag.h\"")
	g.p("")
	g.p("// PSHUFB control: low bytes of the lane's 8 words, then high bytes.")
	g.p("DATA sepmask<>+0(SB)/8, $0x0e0c0a0806040200")
	g.p("DATA sepmask<>+8(SB)/8, $0x0f0d0b0907050301")
	g.p("GLOBL sepmask<>(SB), RODATA|NOPTR, $16")
	g.p("")
	g.p("// Inverse: interleave low and high bytes back into words.")
	g.p("DATA finmask<>+0(SB)/8, $0x0b030a0209010800")
	g.p("DATA finmask<>+8(SB)/8, $0x0f070e060d050c04")
	g.p("GLOBL finmask<>(SB), RODATA|NOPTR, $16")
	for _, c := range []struct {
		k       isa
		name    string
		regions int
	}{{avx2, "AVX2", 6}, {avx512, "AVX512", 8}} {
		g.k = c.k
		g.affineMulti("aff2xMulAdd"+c.name, c.regions, true)
		g.affineMulti("aff2xMul"+c.name, c.regions, false)
		g.affinePrepare("aff2xPrepare"+c.name, false)
		g.affinePrepare("aff2xFinish"+c.name, true)
		g.xorKernel("aff2xXor" + c.name)
	}
	return g.sb.String()
}

const affineOutput = "affine_amd64.s"

func main() {
	surrogate := flag.String("surrogate", "", "write only the surrogate (XOR instead of GF2P8AFFINEQB, WRONG results) affine kernels to this file, which must not be "+affineOutput)
	flag.Parse()
	if os.Getenv("GF16_SURROGATE") != "" {
		fatal("GF16_SURROGATE is no longer supported; use -surrogate=<file other than " + affineOutput + ">")
	}
	if *surrogate != "" {
		if filepath.Base(*surrogate) == affineOutput {
			fatal("-surrogate must not name " + affineOutput)
		}
		write(*surrogate, affineFile(true))
		return
	}
	write("shuffle_amd64.s", shuffleFile())
	write(affineOutput, affineFile(false))
}

func write(name, body string) {
	if err := os.WriteFile(name, []byte(body), 0o644); err != nil {
		fatal(err.Error())
	}
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "asmgen/x86.go:", msg)
	os.Exit(1)
}
