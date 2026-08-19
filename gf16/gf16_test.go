package gf16

import (
	"bytes"
	"math/rand"
	"testing"

	"github.com/javi11/gopar-turbo/gf2p16"
)

func refMul(c uint16, in []byte) []byte {
	out := make([]byte, len(in))
	gf2p16.MulByteSliceLE(gf2p16.T(c), in, out)
	return out
}

func randBytes(t *testing.T, rng *rand.Rand, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	rng.Read(b)
	return b
}

func testCoeffs(rng *rand.Rand) []uint16 {
	cs := []uint16{0, 1, 2, 3, 0x8000, 0xffff}
	for i := 0; i < 8; i++ {
		cs = append(cs, uint16(rng.Intn(1<<16)))
	}
	return cs
}

func TestPrepareFinishRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for _, sliceSize := range []int{4, 64, 4096, 4096 + 2, 65536} {
		ctx, err := NewContext(sliceSize)
		if err != nil {
			t.Fatalf("NewContext(%d): %v", sliceSize, err)
		}
		src := randBytes(t, rng, sliceSize)
		buf := ctx.NewBuffer()
		ctx.Prepare(buf, src)
		out := make([]byte, sliceSize)
		ctx.Finish(buf, out)
		if !bytes.Equal(src, out) {
			t.Errorf("sliceSize=%d: prepare/finish round trip mismatch", sliceSize)
		}
		ctx.Close()
	}
}

func TestMulMatchesReference(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	const sliceSize = 4096
	ctx, err := NewContext(sliceSize)
	if err != nil {
		t.Fatal(err)
	}
	defer ctx.Close()
	src := randBytes(t, rng, sliceSize)
	srcBuf, dstBuf := ctx.NewBuffer(), ctx.NewBuffer()
	ctx.Prepare(srcBuf, src)
	for _, c := range testCoeffs(rng) {
		ctx.Mul(dstBuf, srcBuf, c)
		got := make([]byte, sliceSize)
		ctx.Finish(dstBuf, got)
		// Finish untransforms dstBuf in place, but the next iteration's Mul
		// fully overwrites it, so no restore is needed.
		if want := refMul(c, src); !bytes.Equal(got, want) {
			t.Errorf("coeff %#x: Mul mismatch", c)
		}
	}
}

func TestMulAddMatchesReference(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	const sliceSize = 4096
	ctx, err := NewContext(sliceSize)
	if err != nil {
		t.Fatal(err)
	}
	defer ctx.Close()
	acc := randBytes(t, rng, sliceSize)
	src := randBytes(t, rng, sliceSize)
	for _, c := range testCoeffs(rng) {
		accBuf, srcBuf := ctx.NewBuffer(), ctx.NewBuffer()
		ctx.Prepare(accBuf, acc)
		ctx.Prepare(srcBuf, src)
		ctx.MulAdd(accBuf, srcBuf, c)
		got := make([]byte, sliceSize)
		ctx.Finish(accBuf, got)
		want := append([]byte(nil), acc...)
		gf2p16.MulAndAddByteSliceLE(gf2p16.T(c), src, want)
		if !bytes.Equal(got, want) {
			t.Errorf("coeff %#x: MulAdd mismatch", c)
		}
	}
}

func TestMulAddMultiMatchesReference(t *testing.T) {
	rng := rand.New(rand.NewSource(4))
	const sliceSize, n = 4096, 7
	ctx, err := NewContext(sliceSize)
	if err != nil {
		t.Fatal(err)
	}
	defer ctx.Close()
	srcs := make([][]byte, n)
	bufs := make([][]byte, n)
	coeffs := make([]uint16, n)
	for i := range srcs {
		srcs[i] = randBytes(t, rng, sliceSize)
		bufs[i] = ctx.NewBuffer()
		ctx.Prepare(bufs[i], srcs[i])
		coeffs[i] = uint16(rng.Intn(1 << 16))
	}
	accBuf := ctx.NewBuffer()
	ctx.Prepare(accBuf, make([]byte, sliceSize)) // zero accumulator
	ctx.MulAddMulti(accBuf, bufs, 0, ctx.BufSize(), coeffs)
	got := make([]byte, sliceSize)
	ctx.Finish(accBuf, got)
	want := make([]byte, sliceSize)
	for i := range srcs {
		gf2p16.MulAndAddByteSliceLE(gf2p16.T(coeffs[i]), srcs[i], want)
	}
	if !bytes.Equal(got, want) {
		t.Error("MulAddMulti mismatch")
	}
}

func TestMulAddMultiOffsetRange(t *testing.T) {
	// Exercise the offset/length form used by rsec16's data-range parallelism.
	rng := rand.New(rand.NewSource(5))
	const sliceSize = 8192
	ctx, err := NewContext(sliceSize)
	if err != nil {
		t.Fatal(err)
	}
	defer ctx.Close()
	src := randBytes(t, rng, sliceSize)
	srcBuf, accBuf := ctx.NewBuffer(), ctx.NewBuffer()
	ctx.Prepare(srcBuf, src)
	ctx.Prepare(accBuf, make([]byte, sliceSize))
	half := ctx.BufSize() / 2
	half -= half % ctx.Stride()
	c := uint16(0x1234)
	ctx.MulAddMulti(accBuf, [][]byte{srcBuf}, 0, half, []uint16{c})
	ctx.MulAddMulti(accBuf, [][]byte{srcBuf}, half, ctx.BufSize()-half, []uint16{c})
	got := make([]byte, sliceSize)
	ctx.Finish(accBuf, got)
	if want := refMul(c, src); !bytes.Equal(got, want) {
		t.Error("two-range MulAddMulti != whole-buffer reference")
	}
}

func TestNewContextRejectsBadSizes(t *testing.T) {
	for _, n := range []int{0, -2, 3, 4097} {
		if _, err := NewContext(n); err == nil {
			t.Errorf("NewContext(%d): expected error", n)
		}
	}
}
