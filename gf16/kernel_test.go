package gf16

import (
	"bytes"
	"fmt"
	"math/rand"
	"testing"

	"github.com/javi11/gopar-turbo/gf2p16"
)

// Differential tests: every kernel entry point, for every code path the host
// can run (kernelVariants) plus the pure-Go path, against the gf2p16
// reference.

// testPaths returns the kernels to test; nil is the pure-Go path.
func testPaths() []*kernel {
	return append([]*kernel{nil}, kernelVariants...)
}

func pathName(k *kernel) string {
	if k == nil {
		return "purego"
	}
	return k.name
}

func forEachPath(t *testing.T, fn func(t *testing.T, k *kernel)) {
	for _, k := range testPaths() {
		t.Run(pathName(k), func(t *testing.T) { fn(t, k) })
	}
}

func mustContext(t testing.TB, size int, k *kernel) *Context {
	t.Helper()
	ctx, err := newContext(size, k)
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

var awkwardSizes = []int{2, 4, 30, 32, 34, 62, 64, 66, 94, 100, 1000, 4096, 4098, 65536, 65536 + 30, 300000}

func coeffSet(rng *rand.Rand, extra int) []uint16 {
	cs := []uint16{0, 1, 2, 0x00ff, 0x0100, 0x8000, 0x100b, 0xffff}
	for i := 0; i < extra; i++ {
		cs = append(cs, uint16(rng.Intn(1<<16)))
	}
	return cs
}

func refMulAdd(c uint16, src, dst []byte) {
	gf2p16.MulAndAddByteSliceLE(gf2p16.T(c), src, dst)
}

func randomBuf(rng *rand.Rand, n int) []byte {
	b := make([]byte, n)
	rng.Read(b)
	return b
}

// plainOf returns the whole prepared buffer (padding included) in the
// standard layout.
func plainOf(ctx *Context, buf []byte) []byte {
	out := make([]byte, len(buf))
	if ctx.k == nil {
		copy(out, buf)
	} else {
		ctx.k.finish(&out[0], &buf[0], len(buf))
	}
	return out
}

// preparedOf converts a zero-padded plain buffer of bufSize bytes to the
// prepared layout.
func preparedOf(ctx *Context, plain []byte) []byte {
	buf := ctx.NewBuffer()
	if ctx.k == nil {
		copy(buf, plain)
	} else {
		ctx.k.prepare(&buf[0], &plain[0], len(plain))
	}
	return buf
}

// prepared returns a prepared buffer of src plus the zero-padded plain copy.
func prepared(ctx *Context, src []byte) (buf, padded []byte) {
	buf = ctx.NewBuffer()
	ctx.Prepare(buf, src)
	padded = make([]byte, ctx.BufSize())
	copy(padded, src)
	return buf, padded
}

func TestKernelMulAndMulAdd(t *testing.T) {
	forEachPath(t, func(t *testing.T, k *kernel) {
		rng := rand.New(rand.NewSource(11))
		for _, size := range awkwardSizes {
			ctx := mustContext(t, size, k)
			src := randomBuf(rng, size)
			srcBuf, srcPlain := prepared(ctx, src)
			extra := 6
			if size > 70000 {
				extra = 1
			}
			for _, c := range coeffSet(rng, extra) {
				// Mul
				dst := preparedOf(ctx, randomBuf(rng, ctx.BufSize())) // must be fully overwritten
				ctx.Mul(dst, srcBuf, c)
				want := make([]byte, len(srcPlain))
				gf2p16.MulByteSliceLE(gf2p16.T(c), srcPlain, want)
				if !bytes.Equal(plainOf(ctx, dst), want) {
					t.Fatalf("Mul size=%d c=%#x mismatch", size, c)
				}
				// In-place Mul
				inplace := ctx.NewBuffer()
				copy(inplace, srcBuf)
				ctx.Mul(inplace, inplace, c)
				if !bytes.Equal(plainOf(ctx, inplace), want) {
					t.Fatalf("in-place Mul size=%d c=%#x mismatch", size, c)
				}
				// MulAdd onto random accumulator
				wantAcc := randomBuf(rng, ctx.BufSize())
				acc := preparedOf(ctx, wantAcc)
				refMulAdd(c, srcPlain, wantAcc)
				ctx.MulAdd(acc, srcBuf, c)
				if !bytes.Equal(plainOf(ctx, acc), wantAcc) {
					t.Fatalf("MulAdd size=%d c=%#x mismatch", size, c)
				}
			}
			ctx.Close()
		}
	})
}

func TestKernelPrepareFinish(t *testing.T) {
	forEachPath(t, func(t *testing.T, k *kernel) {
		rng := rand.New(rand.NewSource(12))
		for _, size := range awkwardSizes {
			ctx := mustContext(t, size, k)
			for _, n := range []int{size, max(2, size-2), max(1, size-1)} {
				src := randomBuf(rng, n)
				buf := ctx.NewBuffer()
				rng.Read(buf)
				ctx.Prepare(buf, src)
				out := make([]byte, n)
				before := bytes.Clone(buf)
				ctx.Finish(buf, out)
				if !bytes.Equal(before, buf) {
					t.Fatalf("size=%d n=%d Finish modified its input", size, n)
				}
				if !bytes.Equal(src, out) {
					t.Fatalf("size=%d n=%d round trip mismatch", size, n)
				}
				if plain := plainOf(ctx, buf); !bytes.Equal(plain[n:], make([]byte, len(plain)-n)) {
					t.Fatalf("size=%d n=%d padding not zeroed", size, n)
				}
			}
		}
	})
}

func TestKernelMulAddMulti(t *testing.T) {
	forEachPath(t, func(t *testing.T, k *kernel) {
		rng := rand.New(rand.NewSource(13))
		for _, size := range []int{2, 34, 64, 1000, 8192, 65536 + 30} {
			ctx := mustContext(t, size, k)
			bufSize, stride := ctx.BufSize(), ctx.Stride()
			for _, nsrc := range []int{1, 2, 3, 4, 5, 7, 8, 9, 15, 16, 17, 31, 33} {
				srcs := make([][]byte, nsrc)
				plains := make([][]byte, nsrc)
				coeffs := make([]uint16, nsrc)
				cpool := coeffSet(rng, 20)
				for i := range srcs {
					srcs[i], plains[i] = prepared(ctx, randomBuf(rng, size))
					coeffs[i] = cpool[rng.Intn(len(cpool))]
				}
				ranges := [][2]int{{0, bufSize}}
				if bufSize >= 3*stride {
					ranges = append(ranges, [2]int{stride, bufSize - 2*stride}, [2]int{bufSize - stride, stride})
				}
				for _, r := range ranges {
					offset, length := r[0], r[1]
					want := randomBuf(rng, bufSize)
					acc := preparedOf(ctx, want)
					for i := range srcs {
						refMulAdd(coeffs[i], plains[i][offset:offset+length], want[offset:offset+length])
					}
					ctx.MulAddMulti(acc, srcs, offset, length, coeffs)
					if !bytes.Equal(plainOf(ctx, acc), want) {
						t.Fatalf("size=%d nsrc=%d range=%v mismatch", size, nsrc, r)
					}
				}
			}
		}
	})
}

func TestKernelMulAddMultiZeroAndOneCoefficients(t *testing.T) {
	forEachPath(t, func(t *testing.T, k *kernel) {
		rng := rand.New(rand.NewSource(14))
		ctx := mustContext(t, 4096, k)
		srcs := make([][]byte, 5)
		plains := make([][]byte, 5)
		for i := range srcs {
			srcs[i], plains[i] = prepared(ctx, randomBuf(rng, 4096))
		}
		for _, coeffs := range [][]uint16{{0, 0, 0, 0, 0}, {1, 1, 1, 1, 1}, {0xffff, 0, 1, 0, 0xffff}} {
			want := randomBuf(rng, ctx.BufSize())
			acc := preparedOf(ctx, want)
			for i := range srcs {
				refMulAdd(coeffs[i], plains[i], want)
			}
			ctx.MulAddMulti(acc, srcs, 0, ctx.BufSize(), coeffs)
			if !bytes.Equal(plainOf(ctx, acc), want) {
				t.Fatalf("coeffs %v mismatch", coeffs)
			}
		}
	})
}

// refBank models a bank of prepared rows as plain byte slices.
type refBank [][]byte

func (b refBank) apply(ops []Operation) {
	for _, op := range ops {
		switch op.Kind {
		case OpAdd:
			refMulAdd(op.Coeff, bytes.Clone(b[op.Src]), b[op.Dst])
		case OpScale:
			out := make([]byte, len(b[op.Dst]))
			gf2p16.MulByteSliceLE(gf2p16.T(op.Coeff), b[op.Src], out)
			copy(b[op.Dst], out)
		case OpClear:
			clear(b[op.Dst])
		case OpButterfly:
			refMulAdd(op.Coeff, b[op.Src], b[op.Dst])
			for i := range b[op.Src] {
				b[op.Src][i] ^= b[op.Dst][i]
			}
		case OpInverseButterfly:
			for i := range b[op.Src] {
				b[op.Src][i] ^= b[op.Dst][i]
			}
			refMulAdd(op.Coeff, b[op.Src], b[op.Dst])
		}
	}
}

func randomOps(rng *rand.Rand, rows, count int) []Operation {
	ops := make([]Operation, 0, count)
	cpool := coeffSet(rng, 4)
	for len(ops) < count {
		kind := uint16(rng.Intn(int(OpInverseButterfly) + 1))
		op := Operation{Dst: uint32(rng.Intn(rows)), Src: uint32(rng.Intn(rows)), Coeff: cpool[rng.Intn(len(cpool))], Kind: kind}
		if kind >= OpButterfly && op.Dst == op.Src {
			continue
		}
		ops = append(ops, op)
	}
	return ops
}

// adjacentRuns builds operations on consecutive rows with one coefficient so
// the executor's run coalescing is exercised.
func adjacentRuns(rng *rand.Rand, rows int) []Operation {
	var ops []Operation
	half := rows / 2
	for _, kind := range []uint16{OpAdd, OpButterfly, OpInverseButterfly, OpClear, OpAdd} {
		c := uint16(rng.Intn(1 << 16))
		start := rng.Intn(half)
		n := 1 + rng.Intn(half-start)
		for r := 0; r < n; r++ {
			ops = append(ops, Operation{Dst: uint32(start + r), Src: uint32(half + start + r), Coeff: c, Kind: kind})
		}
	}
	return ops
}

func TestKernelBufferSetOperations(t *testing.T) {
	forEachPath(t, func(t *testing.T, k *kernel) {
		rng := rand.New(rand.NewSource(15))
		for _, size := range []int{2, 34, 64, 1000, 4096} {
			const rows = 12
			for round := 0; round < 6; round++ {
				ctx := mustContext(t, size, k)
				set, err := NewBufferWorkspace(ctx, rows)
				if err != nil {
					t.Fatal(err)
				}
				bank := make(refBank, rows)
				for i, row := range set.Buffers() {
					_, plain := prepared(ctx, randomBuf(rng, size))
					copy(row, preparedOf(ctx, plain))
					bank[i] = bytes.Clone(plain)
				}
				var ops []Operation
				if round%2 == 0 {
					ops = randomOps(rng, rows, 40)
				} else {
					ops = adjacentRuns(rng, rows)
				}
				if err := set.Apply(ctx, ops); err != nil {
					t.Fatal(err)
				}
				bank.apply(ops)
				for i, row := range set.Buffers() {
					if !bytes.Equal(plainOf(ctx, row), bank[i]) {
						t.Fatalf("size=%d round=%d row %d mismatch", size, round, i)
					}
				}
				set.Close()
			}
		}
	})
}

func TestKernelScaleAliasing(t *testing.T) {
	forEachPath(t, func(t *testing.T, k *kernel) {
		if k == nil {
			t.Skip("partial overlap needs the kernel path")
		}
		rng := rand.New(rand.NewSource(16))
		ctx := mustContext(t, 256, k)
		bank := ctx.NewBuffers(2)
		_, plain := prepared(ctx, randomBuf(rng, 256))
		copy(bank[0], preparedOf(ctx, plain))
		set, err := NewBufferSet(ctx, bank)
		if err != nil {
			t.Fatal(err)
		}
		defer set.Close()
		// In-place scale.
		want := make([]byte, len(plain))
		gf2p16.MulByteSliceLE(gf2p16.T(0x1234), plain, want)
		if err := set.Apply(ctx, []Operation{{Dst: 0, Src: 0, Coeff: 0x1234, Kind: OpScale}}); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(plainOf(ctx, bank[0]), want) {
			t.Fatal("in-place scale mismatch")
		}
	})
}

func TestKernelNameAndGeometry(t *testing.T) {
	for _, k := range kernelVariants {
		ctx := mustContext(t, 100, k)
		if ctx.MethodName() != k.name || ctx.Stride() != k.stride || ctx.BufSize()%k.stride != 0 || ctx.BufSize() < 100 {
			t.Errorf("%s: bad geometry method=%q stride=%d buf=%d", k.name, ctx.MethodName(), ctx.Stride(), ctx.BufSize())
		}
	}
	if Accelerated() != (native != nil) {
		t.Error("Accelerated disagrees with native kernel")
	}
}

func FuzzKernelMulAdd(f *testing.F) {
	f.Add([]byte{1, 2, 3, 4, 5, 6, 7, 8}, uint16(0x1234), uint8(3))
	f.Add(bytes.Repeat([]byte{0xff}, 70), uint16(0xffff), uint8(0))
	f.Fuzz(func(t *testing.T, data []byte, coeff uint16, nsrcRaw uint8) {
		if len(data) < 2 {
			return
		}
		data = data[:len(data)&^1]
		nsrc := 1 + int(nsrcRaw)%(maxPassRegions+2)
		for _, k := range kernelVariants {
			ctx := mustContext(t, len(data), k)
			srcs := make([][]byte, nsrc)
			want := make([]byte, ctx.BufSize())
			coeffs := make([]uint16, nsrc)
			for i := range srcs {
				rotated := append(bytes.Clone(data[i%len(data)&^1:]), data[:i%len(data)&^1]...)
				var plain []byte
				srcs[i], plain = prepared(ctx, rotated)
				coeffs[i] = coeff + uint16(i)*0x0101
				refMulAdd(coeffs[i], plain, want)
			}
			acc := ctx.NewBuffer()
			ctx.MulAddMulti(acc, srcs, 0, ctx.BufSize(), coeffs)
			if !bytes.Equal(plainOf(ctx, acc), want) {
				t.Fatalf("%s: len=%d nsrc=%d mismatch", k.name, len(data), nsrc)
			}
		}
	})
}

func ExampleContext_MulAddMulti() {
	ctx, _ := NewContext(64)
	src, acc := ctx.NewBuffer(), ctx.NewBuffer()
	ctx.Prepare(src, bytes.Repeat([]byte{1, 0}, 32))
	ctx.MulAddMulti(acc, [][]byte{src}, 0, ctx.BufSize(), []uint16{7})
	out := make([]byte, 64)
	ctx.Finish(acc, out)
	fmt.Println(out[:2])
	// Output: [7 0]
}
