package gf16

import (
	"bytes"
	"math/rand"
	"testing"

	"github.com/javi11/gopar-turbo/gf2p16"
)

// TestKernelUnalignedBuffers runs every kernel entry point with source and
// destination at all relative byte offsets 1..31 (alignment is only a hint).
func TestKernelUnalignedBuffers(t *testing.T) {
	for _, k := range kernelVariants {
		t.Run(k.name, func(t *testing.T) {
			rng := rand.New(rand.NewSource(4))
			n := 4 * k.stride
			plain := func(buf []byte) []byte {
				out := make([]byte, n)
				k.finish(&out[0], &buf[0], n)
				return out
			}
			for so := 1; so < 32; so++ {
				for do := 1; do < 32; do++ {
					sbase, dbase := randomBuf(rng, n+64), randomBuf(rng, n+64)
					src, dst := sbase[so:so+n:so+n], dbase[do:do+n:do+n]
					c := uint16(rng.Intn(65534) + 2)
					sp := plain(src)

					want := make([]byte, n)
					gf2p16.MulByteSliceLE(gf2p16.T(c), sp, want)
					k.mul(&dst[0], &src[0], n, c)
					if !bytes.Equal(plain(dst), want) {
						t.Fatalf("mul so=%d do=%d", so, do)
					}
					want = plain(dst)
					gf2p16.MulAndAddByteSliceLE(gf2p16.T(c), sp, want)
					k.mulAdd(&dst[0], &src[0], n, c)
					if !bytes.Equal(plain(dst), want) {
						t.Fatalf("mulAdd so=%d do=%d", so, do)
					}
					want = plain(dst)
					gf2p16.MulAndAddByteSliceLE(gf2p16.T(c), sp, want)
					gf2p16.MulAndAddByteSliceLE(gf2p16.T(c^0x55), sp, want)
					ptrs, cs := srcPtrs{&src[0], &src[0]}, coefList{c, c ^ 0x55}
					k.mulAddMulti(&dst[0], &ptrs, &cs, 2, n)
					if !bytes.Equal(plain(dst), want) {
						t.Fatalf("mulAddMulti so=%d do=%d", so, do)
					}
					want = plain(dst)
					for i, b := range sp {
						want[i] ^= b
					}
					k.xor(&dst[0], &src[0], n)
					if !bytes.Equal(plain(dst), want) {
						t.Fatalf("xor so=%d do=%d", so, do)
					}
					k.prepare(&dst[0], &src[0], n)
					if !bytes.Equal(plain(dst), src) {
						t.Fatalf("prepare so=%d do=%d", so, do)
					}
				}
			}
		})
	}
}

// TestContextUnalignedBuffers exercises the Context glue on unaligned slices.
func TestContextUnalignedBuffers(t *testing.T) {
	forEachPath(t, func(t *testing.T, k *kernel) {
		rng := rand.New(rand.NewSource(5))
		ctx := mustContext(t, 4096, k)
		size := ctx.BufSize()
		for off := 1; off < 32; off++ {
			src := randomBuf(rng, size+64)[off : off+4096]
			sbuf := make([]byte, size+64)[off : off+size]
			ctx.Prepare(sbuf, src)
			accPlain := randomBuf(rng, size)
			acc := make([]byte, size+64)[off/2 : off/2+size]
			ctx.k0Prepare(acc, accPlain)
			full := make([]byte, size)
			copy(full, src)
			refMulAdd(0x1357, full, accPlain)
			ctx.MulAdd(acc, sbuf, 0x1357)
			if !bytes.Equal(plainOf(ctx, acc), accPlain) {
				t.Fatalf("off=%d MulAdd", off)
			}
		}
	})
}

// k0Prepare converts a whole zero-padded plain buffer into the prepared layout.
func (c *Context) k0Prepare(dst, plain []byte) {
	if c.k == nil {
		copy(dst, plain)
		return
	}
	c.k.prepare(&dst[0], &plain[0], len(plain))
}

// TestCoalescingStress applies random operation lists on banks whose rows are
// shuffled (index-adjacent is not address-adjacent), contiguous, and
// overlapping runs, against a sequential plain-layout reference.
func TestCoalescingStress(t *testing.T) {
	forEachPath(t, func(t *testing.T, k *kernel) {
		rng := rand.New(rand.NewSource(77))
		for round := 0; round < 600; round++ {
			size := []int{2, 34, 64, 100}[rng.Intn(4)]
			ctx := mustContext(t, size, k)
			const rows = 16
			bank := ctx.NewBuffers(rows)
			if round%3 == 0 {
				rng.Shuffle(rows, func(i, j int) { bank[i], bank[j] = bank[j], bank[i] })
			}
			set, err := NewBufferSet(ctx, bank)
			if err != nil {
				t.Fatal(err)
			}
			ref := make(refBank, rows)
			for i := range bank {
				_, plain := prepared(ctx, randomBuf(rng, size))
				copy(bank[i], preparedOf(ctx, plain))
				ref[i] = bytes.Clone(plain)
			}
			var ops []Operation
			cp := coeffSet(rng, 2)
			for len(ops) < 30 {
				kind := uint16(rng.Intn(5))
				c := cp[rng.Intn(len(cp))]
				n := 1 + rng.Intn(6)
				d, s := rng.Intn(rows-n+1), rng.Intn(rows-n+1)
				for r := 0; r < n; r++ {
					op := Operation{Dst: uint32(d + r), Src: uint32(s + r), Coeff: c, Kind: kind}
					if kind >= OpButterfly && op.Dst == op.Src {
						continue
					}
					ops = append(ops, op)
				}
			}
			if err := set.Apply(ctx, ops); err != nil {
				t.Fatal(err)
			}
			ref.apply(ops)
			for i := range bank {
				if !bytes.Equal(plainOf(ctx, bank[i]), ref[i]) {
					t.Fatalf("round=%d size=%d row %d mismatch", round, size, i)
				}
			}
			set.Close()
		}
	})
}

// TestOpScaleScratchAndPureGo covers OpScale for in-place, disjoint and (on
// the kernel path) partially overlapping rows, which goes through the
// context scratch buffer; the pure-Go path always does.
func TestOpScaleScratchAndPureGo(t *testing.T) {
	forEachPath(t, func(t *testing.T, k *kernel) {
		rng := rand.New(rand.NewSource(6))
		ctx := mustContext(t, 256, k)
		size := ctx.BufSize()
		// A backing array holding rows at aligned, partially overlapping
		// offsets: row i starts i*align bytes in.
		step := ctx.align
		const nrows = 3
		backing := alignedSlice(size+nrows*step, ctx.align)
		rowsB := make([][]byte, nrows)
		for i := range rowsB {
			rowsB[i] = backing[i*step : i*step+size : i*step+size]
		}
		set, err := NewBufferSet(ctx, rowsB)
		if err != nil {
			t.Fatal(err)
		}
		defer set.Close()
		cases := []Operation{
			{Dst: 0, Src: 0, Coeff: 0x1234, Kind: OpScale}, // in place
			{Dst: 0, Src: 1, Coeff: 0x4321, Kind: OpScale}, // partial overlap
			{Dst: 2, Src: 1, Coeff: 0xffff, Kind: OpScale}, // partial overlap, other way
			{Dst: 1, Src: 0, Coeff: 0, Kind: OpScale},      // zero coefficient
			{Dst: 1, Src: 2, Coeff: 1, Kind: OpScale},
		}
		for _, op := range cases {
			rng.Read(backing)
			// Reference on the prepared bytes in plain layout: read the
			// source rows as they are now, write dst row with snapshot
			// semantics.
			srcPlain := plainOf(ctx, bytes.Clone(rowsB[op.Src]))
			want := make([]byte, size)
			gf2p16.MulByteSliceLE(gf2p16.T(op.Coeff), srcPlain, want)
			before := bytes.Clone(backing)
			if err := set.Apply(ctx, []Operation{op}); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(plainOf(ctx, bytes.Clone(rowsB[op.Dst])), want) {
				t.Fatalf("%+v: scale result mismatch", op)
			}
			// Bytes outside the destination row must be untouched.
			lo := int(op.Dst) * step
			if !bytes.Equal(backing[:lo], before[:lo]) || !bytes.Equal(backing[lo+size:], before[lo+size:]) {
				t.Fatalf("%+v: bytes outside destination modified", op)
			}
		}
	})
}

func TestChooseKernel(t *testing.T) {
	a := &kernel{name: "A", stride: 32}
	b := &kernel{name: "B", stride: 32}
	cases := []struct {
		name string
		want *kernel
	}{
		{"", a}, {"B", b}, {"A", a}, {pureGoKernelName, nil},
		{"unknown", a}, {"b", a}, {"CLMul (NEON) ", a},
	}
	for _, c := range cases {
		if got := chooseKernel(c.name, a, []*kernel{a, b}); got != c.want {
			t.Errorf("chooseKernel(%q) = %v, want %v", c.name, got, c.want)
		}
	}
	// A kernel the CPU cannot run is not in variants, so it is ignored.
	if got := chooseKernel("C", a, []*kernel{a}); got != a {
		t.Error("unsupported kernel name must keep the default")
	}
}

// TestPrintKernels logs the kernel chosen by default and every variant this
// CPU can run; CI uses it to show which kernels a job exercised.
func TestPrintKernels(t *testing.T) {
	ctx := mustContext(t, 64, native)
	t.Logf("default kernel: %s (accelerated=%v)", ctx.MethodName(), Accelerated())
	for _, k := range kernelVariants {
		t.Logf("runnable variant: %s", k.name)
	}
}
