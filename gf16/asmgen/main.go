//go:build ignore

// asmgen emits clmul_arm64.s: the NEON/PMULL GF(2^16) region kernels.
//
// Derived from ParPar's CLMul methods (CC0); see gf16/LICENSE.parpar.
//
// Run it with `go generate ./gf16` (see gf16/clmul_arm64.go). The assembly is
// generated rather than hand written because each kernel comes in several
// flavours (with and without SHA3 EOR3, multiply-add vs plain multiply) and
// unrolls over up to sixteen source regions.
//
// # The math
//
// A prepared region is split into 32-byte blocks holding 16 little-endian
// 16-bit words in planar form: the 16 low bytes (D0) then the 16 high bytes
// (D1), which is what prepareBlocks/finishBlocks convert to and from. One LDP
// loads a block straight into D0 and D1 (a plain-layout block would need a
// de-interleaving LD2, measurably slower on Apple cores). With the coefficient c = ch*x^8 + cl
// split into bytes, a word w = wh*x^8 + wl multiplies as (all products are
// carry-less 8x8 -> 16 bit, PMULL):
//
//	w*c = wl*cl  +  ((wl^wh)*(cl^ch) ^ wl*cl ^ wh*ch) * x^8  +  wh*ch * x^16
//
// (Karatsuba: three PMULLs instead of four.) Each PMULL yields eight 16-bit
// products from one 64-bit half, so per block and per source we issue six:
// low/mid/high for the low and the high half of the 16 lanes. Products of all
// sources are XOR-accumulated in six registers (low1 low2 mid1 mid2 high1
// high2) and the polynomial reduction (modulo x^16+x^12+x^3+x+1, ParPar's
// 0x1100b) runs once per block, not once per source. That packing is what
// makes multi-source passes much cheaper than repeated single-source ones.
//
// # Accumulation
//
// Each product is an adjacent "PMULL Vt; EOR Vt, Vt, Vacc" pair writing a
// new register that becomes the accumulator. Apple cores fuse that pair into
// one operation, and it needs no temporary, at the price of two register
// banks (A and B) that successive sources alternate between. Which bank the
// first source initialises depends on the parity of the source count so that
// the last source always lands in bank A: the kernel has an odd-count body
// and an even-count body.
//
// # Registers
//
//	R0 dst (advances)  R1 source-pointer array  R2 coefficients  R3 nsrc
//	R4 bytes left      R5 coefficient table (stack frame)
//	R6-R17,R19-R22 source pointers (each advances 32 bytes per block)
//	R23 table cursor, R24 table base of regions 4-15
//	V0,V1 source data  V2 D0^D1
//	V4-V15 coefficients of regions 0-3 (clo, chi, clo^chi each, byte-broadcast)
//	V16-V18 coefficients of regions 4-15, reloaded from the table per use
//	V19 reduction polynomial 0x0b
//	V20-V25 accumulator bank A (low1 low2 mid1 mid2 high1 high2)
//	V26-V31 accumulator bank B (reduction scratch once the sum is in A)
package main

import (
	"bytes"
	"fmt"
	"os"
)

const (
	maxRegions   = 8  // must match maxPassRegions in kernel.go
	residentRegs = 4  // regions whose coefficients live in registers
	tableBytes   = 48 // three 16-byte coefficient vectors per region
	blockBytes   = 32
	vectorBytes  = 16
	firstCoefReg = 4  // V4: coefficients of region 0
	scratchCoef  = 16 // V16-V18: coefficients of a non-resident region
	polyReg      = 19
	tableCursor  = 23 // R23: walks the non-resident part of the table per block
	tableBase    = 24 // R24: start of the non-resident part of the table
)

// srcPtrReg returns the scalar register holding region k's source pointer:
// R6-R17 then R19-R22 (R18 is reserved by some platforms).
func srcPtrReg(k int) int {
	switch {
	case k < 12:
		return 6 + k
	default:
		return 19 + k - 12
	}
}

var (
	bankA = [6]int{20, 21, 22, 23, 24, 25}
	bankB = [6]int{26, 27, 28, 29, 30, 31}
)

type flavour struct {
	suffix string
	eor3   bool // use SHA3 EOR3 for 3-way XORs
	add    bool // multiply-add into dst (else overwrite dst)
}

var buf bytes.Buffer

func p(format string, args ...any) { fmt.Fprintf(&buf, format+"\n", args...) }

// xor3 emits d = a ^ b ^ c. Without EOR3, d must not be c.
func (f flavour) xor3(a, b, c, d int) {
	if f.eor3 {
		p("\tVEOR3 V%d.B16, V%d.B16, V%d.B16, V%d.B16", a, b, c, d)
		return
	}
	p("\tVEOR V%d.B16, V%d.B16, V%d.B16", a, b, d)
	p("\tVEOR V%d.B16, V%d.B16, V%d.B16", c, d, d)
}

// pmul emits PMUL Vd.16B, Vn.16B, Vm.16B (8-bit polynomial multiply, low
// byte of each product). The Go assembler has no mnemonic for it.
func pmul(n, m, d int) {
	enc := uint32(0x6E209C00) | uint32(m)<<16 | uint32(n)<<5 | uint32(d)
	p("\tWORD $0x%08x // PMUL V%d.16B, V%d.16B, V%d.16B", enc, d, n, m)
}

// coefRegs returns the registers holding clo, chi, clo^chi for region k, and
// whether a table load is needed first.
func coefRegs(k int) (clo, chi, cmid int, load bool) {
	if k < residentRegs {
		b := firstCoefReg + 3*k
		return b, b + 1, b + 2, false
	}
	return scratchCoef, scratchCoef + 1, scratchCoef + 2, true
}

// region emits the multiply of one source block by region k's coefficient.
// from is the accumulator bank to extend, or nil to initialise to; to is the
// bank written.
func region(k int, from, to *[6]int) {
	clo, chi, cmid, load := coefRegs(k)
	p("\tFLDPQ.P %d(R%d), (F0, F1)", blockBytes, srcPtrReg(k))
	if load {
		p("\tVLD1.P %d(R%d), [V%d.B16, V%d.B16, V%d.B16]", tableBytes, tableCursor, clo, chi, cmid)
	}
	p("\tVEOR V0.B16, V1.B16, V2.B16")
	type prod struct {
		hi        bool
		data, cof int
	}
	prods := [6]prod{
		{false, 0, clo}, {true, 0, clo}, // low  = D0*clo
		{false, 2, cmid}, {true, 2, cmid}, // mid  = (D0^D1)*(clo^chi)
		{false, 1, chi}, {true, 1, chi}, // high = D1*chi
	}
	for i, q := range prods {
		if q.hi {
			p("\tVPMULL2 V%d.B16, V%d.B16, V%d.H8", q.data, q.cof, to[i])
		} else {
			p("\tVPMULL V%d.B8, V%d.B8, V%d.H8", q.data, q.cof, to[i])
		}
		if from != nil {
			p("\tVEOR V%d.B16, V%d.B16, V%d.B16", to[i], from[i], to[i])
		}
	}
}

// reduce emits the Barrett-style reduction of bank A and the merge with dst,
// storing the finished block at R0 and advancing R0.
func (f flavour) reduce() {
	lo1, lo2, mid1, mid2, hi1, hi2 := bankA[0], bankA[1], bankA[2], bankA[3], bankA[4], bankA[5]
	h0, h1, l0, l1, m0 := bankB[0], bankB[1], bankB[2], bankB[3], bankB[4]
	m1 := lo1
	p("\t// de-interleave 16-bit products into byte planes")
	p("\tVUZP1 V%d.B16, V%d.B16, V%d.B16 // H0 = even bytes of high", hi2, hi1, h0)
	p("\tVUZP2 V%d.B16, V%d.B16, V%d.B16 // H1 = odd bytes of high", hi2, hi1, h1)
	p("\tVUZP1 V%d.B16, V%d.B16, V%d.B16 // L0", lo2, lo1, l0)
	p("\tVUZP2 V%d.B16, V%d.B16, V%d.B16 // L1", lo2, lo1, l1)
	p("\tVUZP1 V%d.B16, V%d.B16, V%d.B16 // M0", mid2, mid1, m0)
	p("\tVUZP2 V%d.B16, V%d.B16, V%d.B16 // M1", mid2, mid1, m1)
	p("\t// fold the Karatsuba middle term: lib = H0^L1; L1 = lib^L0^M0; H0 = lib^H1^M1")
	lib := lo2
	p("\tVEOR V%d.B16, V%d.B16, V%d.B16", l1, h0, lib)
	f.xor3(lib, l0, m0, l1)
	f.xor3(lib, h1, m1, h0)
	p("\t// reduce by 0x1100b: th0 = (H1<<4 | H0>>4) ^ th1 ^ H0, th1 = H1 ^ H1>>4")
	th0, th1, hi3, hi1b := mid1, mid2, hi1, hi2
	p("\tVSHL $4, V%d.B16, V%d.B16", h1, th0)
	p("\tVSRI $4, V%d.B16, V%d.B16", h0, th0)
	p("\tVUSHR $4, V%d.B16, V%d.B16", h1, th1)
	p("\tVEOR V%d.B16, V%d.B16, V%d.B16", th1, h1, th1)
	f.xor3(th0, th1, h0, th0)
	p("\tVUSHR $5, V%d.B16, V%d.B16 // th0_hi3", th0, hi3)
	p("\tVUSHR $2, V%d.B16, V%d.B16 // th0_hi1", hi3, hi1b)
	p("\tVUSHR $5, V%d.B16, V%d.B16", h1, lib)
	p("\tVEOR V%d.B16, V%d.B16, V%d.B16", lib, th0, th0)
	p("\tVSLI $4, V%d.B16, V%d.B16", th0, hi3)
	pmul(th1, polyReg, th1)
	pmul(th0, polyReg, th0)
	f.xor3(hi3, hi1b, th1, hi3)
	// out0 = L0 ^ th0 (^ dst0) into l0; out1 = hi3 ^ L1 (^ dst1) into l1.
	if f.add {
		p("\tFLDPQ (R0), (F0, F1)")
		f.xor3(l0, th0, 0, l0)
		f.xor3(hi3, l1, 1, l1)
	} else {
		p("\tVEOR V%d.B16, V%d.B16, V%d.B16", l0, th0, l0)
		p("\tVEOR V%d.B16, V%d.B16, V%d.B16", hi3, l1, l1)
	}
	p("\tFSTPQ.P (F%d, F%d), %d(R0)", l0, l1, blockBytes)
}

// body emits the block loop for an odd or even source count. Sources are
// processed in order; the bank written alternates, and the loop may exit to
// the reduction only when the running sum is in bank A.
func (f flavour) body(odd bool) {
	tag, cur, other := "even", &bankB, &bankA
	if odd {
		tag, cur, other = "odd", &bankA, &bankB
	}
	p("loop_%s:", tag)
	p("\tMOVD R%d, R%d", tableBase, tableCursor)
	region(0, nil, cur)
	for k := 1; k < maxRegions; k++ {
		if cur == &bankA {
			p("\tCMP $%d, R3", k)
			p("\tBEQ reduce_%s", tag)
		}
		region(k, cur, other)
		cur, other = other, cur
	}
	p("reduce_%s:", tag)
	f.reduce()
	// SUBS/BHI (unsigned R4 > blockBytes) instead of SUB/CBNZ: a length that
	// is not a block multiple ends the loop instead of running off the buffer.
	p("\tSUBS $%d, R4, R4", blockBytes)
	p("\tBHI loop_%s", tag)
	p("\tRET")
}

func (f flavour) emit() {
	name := "clmulMul"
	if f.add {
		name = "clmulMulAdd"
	}
	name += f.suffix
	p("// func %s(dst *byte, srcs *[%d]*byte, coeffs *[%d]uint16, nsrc, n int)", name, maxRegions, maxRegions)
	p("TEXT ·%s(SB), $%d-40", name, maxRegions*tableBytes)
	p("\tMOVD dst+0(FP), R0")
	p("\tMOVD srcs+8(FP), R1")
	p("\tMOVD coeffs+16(FP), R2")
	p("\tMOVD nsrc+24(FP), R3")
	p("\tMOVD n+32(FP), R4")
	p("\tCBZ R4, done")
	p("\tCBZ R3, done")
	p("\t// build the coefficient table: per source clo, chi, clo^chi broadcast")
	p("\tADD $8, RSP, R5")
	p("\tMOVD R5, R6")
	p("\tMOVD R3, R7")
	p("tbl:")
	p("\tMOVHU.P 2(R2), R8")
	p("\tAND $0xff, R8, R9")
	p("\tLSR $8, R8, R10")
	p("\tEOR R9, R10, R11")
	p("\tVDUP R9, V16.B16")
	p("\tVDUP R10, V17.B16")
	p("\tVDUP R11, V18.B16")
	p("\tVST1.P [V16.B16, V17.B16, V18.B16], %d(R6)", tableBytes)
	p("\tSUB $1, R7")
	p("\tCBNZ R7, tbl")
	p("\tMOVD $0x0b, R7")
	p("\tVDUP R7, V%d.B16", polyReg)
	p("\t// resident coefficients: regions 0-%d (table slots past nsrc are harmless)", residentRegs-1)
	for k := 0; k < residentRegs; k++ {
		clo, chi, cmid, _ := coefRegs(k)
		p("\tVLD1.P %d(R5), [V%d.B16, V%d.B16, V%d.B16]", tableBytes, clo, chi, cmid)
	}
	p("\tMOVD R5, R%d", tableBase)
	p("\t// source pointers (array slots past nsrc are never dereferenced)")
	for k := 0; k < maxRegions; k += 2 {
		p("\tLDP.P 16(R1), (R%d, R%d)", srcPtrReg(k), srcPtrReg(k+1))
	}
	p("\tTBZ $0, R3, even")
	f.body(true)
	p("even:")
	f.body(false)
	p("done:")
	p("\tRET")
	p("")
}

func main() {
	p("// Code generated by asmgen/main.go; DO NOT EDIT.")
	p("// Derived from the algorithms of ParPar (https://github.com/animetosho/ParPar, CC0); see gf16/LICENSE.parpar.")
	p("")
	p("#include \"textflag.h\"")
	p("")
	for _, add := range []bool{true, false} {
		for _, eor3 := range []bool{false, true} {
			s := "NEON"
			if eor3 {
				s = "SHA3"
			}
			flavour{suffix: s, eor3: eor3, add: add}.emit()
		}
	}
	if err := os.WriteFile("clmul_arm64.s", buf.Bytes(), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
