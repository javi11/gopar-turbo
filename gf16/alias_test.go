package gf16

import (
	"bytes"
	"fmt"
	"math/rand"
	"testing"
)

// aliasCoeffs builds nsrc coefficients. pattern 0: all non-zero; pattern 1:
// every third one zero (phase shifted by shift) so pass boundaries move;
// pattern 2: mostly zero with a few survivors.
func aliasCoeffs(rng *rand.Rand, nsrc, pattern, shift int) []uint16 {
	cs := make([]uint16, nsrc)
	for i := range cs {
		cs[i] = uint16(rng.Intn(65535) + 1)
		switch pattern {
		case 1:
			if (i+shift)%3 == 0 {
				cs[i] = 0
			}
		case 2:
			if (i+shift)%5 != 0 {
				cs[i] = 0
			}
		}
	}
	return cs
}

// TestMulAddMultiAliasing checks dst ^= sum c_i*snapshot(src_i) when dst is
// also a source, at every position, for every source count, with zero
// coefficients shifting the pass boundaries, and with dst listed repeatedly.
func TestMulAddMultiAliasing(t *testing.T) {
	forEachPath(t, func(t *testing.T, k *kernel) {
		rng := rand.New(rand.NewSource(21))
		ctx := mustContext(t, 200, k)
		size := ctx.BufSize()
		for nsrc := 1; nsrc <= 33; nsrc++ {
			for pos := 0; pos < nsrc; pos++ {
				for pattern := 0; pattern < 3; pattern++ {
					for _, twice := range []bool{false, true} {
						pos2 := -1
						if twice {
							if nsrc < 2 {
								continue
							}
							pos2 = (pos + 1 + nsrc/2) % nsrc
							if pos2 == pos {
								continue
							}
						}
						name := fmt.Sprintf("nsrc=%d pos=%d pattern=%d pos2=%d", nsrc, pos, pattern, pos2)
						srcs := make([][]byte, nsrc)
						plains := make([][]byte, nsrc)
						for i := range srcs {
							srcs[i], plains[i] = prepared(ctx, randomBuf(rng, 200))
						}
						dst := srcs[pos]
						srcs[pos] = dst
						if twice {
							srcs[pos2], plains[pos2] = dst, plains[pos]
						}
						cs := aliasCoeffs(rng, nsrc, pattern, pos)
						want := bytes.Clone(plains[pos]) // snapshot of dst
						for i := range srcs {
							refMulAdd(cs[i], plains[i], want)
						}
						ctx.MulAddMulti(dst, srcs, 0, size, cs)
						if !bytes.Equal(plainOf(ctx, dst), want) {
							t.Fatalf("%s: wrong result", name)
						}
					}
				}
			}
		}
	})
}

// TestMulAddMultiAliasingRange checks the aliasing result inside a sub-range
// and that bytes outside the range are untouched.
func TestMulAddMultiAliasingRange(t *testing.T) {
	forEachPath(t, func(t *testing.T, k *kernel) {
		rng := rand.New(rand.NewSource(22))
		ctx := mustContext(t, 512, k)
		stride, size := ctx.Stride(), ctx.BufSize()
		if size < 3*stride {
			t.Skip("buffer too small for a sub-range")
		}
		offset, length := stride, size-2*stride
		for _, nsrc := range []int{2, 9, 13, 17} {
			srcs := make([][]byte, nsrc)
			plains := make([][]byte, nsrc)
			for i := range srcs {
				srcs[i], plains[i] = prepared(ctx, randomBuf(rng, 512))
			}
			dst := srcs[nsrc-2]
			cs := aliasCoeffs(rng, nsrc, 0, 0)
			want := bytes.Clone(plains[nsrc-2])
			for i := range srcs {
				refMulAdd(cs[i], plains[i][offset:offset+length], want[offset:offset+length])
			}
			ctx.MulAddMulti(dst, srcs, offset, length, cs)
			if !bytes.Equal(plainOf(ctx, dst), want) {
				t.Fatalf("nsrc=%d: wrong result", nsrc)
			}
		}
	})
}

func mustPanic(t *testing.T, what string, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Errorf("%s: expected a panic", what)
		}
	}()
	fn()
}

// TestMulAddMultiValidatesBeforeWriting checks that a bad argument anywhere
// in the list panics before dst is modified (earlier sources used to be
// accumulated first) and leaves no pointers in the context scratch.
func TestMulAddMultiValidatesBeforeWriting(t *testing.T) {
	forEachPath(t, func(t *testing.T, k *kernel) {
		rng := rand.New(rand.NewSource(23))
		ctx := mustContext(t, 256, k)
		size := ctx.BufSize()
		for _, nsrc := range []int{3, 12, 20} {
			for _, badAt := range []int{0, nsrc / 2, nsrc - 1} {
				srcs := make([][]byte, nsrc)
				cs := make([]uint16, nsrc)
				for i := range srcs {
					srcs[i], _ = prepared(ctx, randomBuf(rng, 256))
					cs[i] = uint16(rng.Intn(65535) + 1)
				}
				srcs[badAt] = srcs[badAt][:size-ctx.Stride()]
				dst, _ := prepared(ctx, randomBuf(rng, 256))
				before := bytes.Clone(dst)
				mustPanic(t, "short source", func() { ctx.MulAddMulti(dst, srcs, 0, size, cs) })
				if !bytes.Equal(dst, before) {
					t.Fatalf("nsrc=%d badAt=%d: dst modified before validation failed", nsrc, badAt)
				}
				if ctx.ptrs != (srcPtrs{}) {
					t.Fatalf("nsrc=%d badAt=%d: scratch pointers left behind", nsrc, badAt)
				}
			}
		}
		// Argument errors that do not depend on the sources.
		dst, _ := prepared(ctx, randomBuf(rng, 256))
		src, _ := prepared(ctx, randomBuf(rng, 256))
		mustPanic(t, "coeff count", func() { ctx.MulAddMulti(dst, [][]byte{src}, 0, size, nil) })
		mustPanic(t, "range", func() { ctx.MulAddMulti(dst, [][]byte{src}, 0, size+ctx.Stride(), []uint16{3}) })
		if k != nil {
			mustPanic(t, "unaligned range", func() { ctx.MulAddMulti(dst, [][]byte{src}, 1, size-1, []uint16{3}) })
		}
		mustPanic(t, "short dst", func() { ctx.MulAddMulti(dst[:size-ctx.Stride()], [][]byte{src}, 0, size, []uint16{3}) })
	})
}

// TestMulAddMultiClearsScratch checks that a successful call does not keep the
// caller's buffers reachable through the context.
func TestMulAddMultiClearsScratch(t *testing.T) {
	forEachPath(t, func(t *testing.T, k *kernel) {
		ctx := mustContext(t, 64, k)
		rng := rand.New(rand.NewSource(24))
		dst, _ := prepared(ctx, randomBuf(rng, 64))
		src, _ := prepared(ctx, randomBuf(rng, 64))
		ctx.MulAddMulti(dst, [][]byte{src, dst, src}, 0, ctx.BufSize(), []uint16{5, 6, 7})
		if ctx.ptrs != (srcPtrs{}) {
			t.Fatal("scratch pointers left behind")
		}
	})
}
