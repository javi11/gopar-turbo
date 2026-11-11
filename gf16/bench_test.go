package gf16

import (
	"fmt"
	"math/rand"
	"testing"
)

// Kernel benchmarks. They use only the public API so the same file measures
// the default backend; force another kernel with GOPAR_GF16_KERNEL.

var benchSizes = []struct {
	name string
	size int
}{{"64K", 64 << 10}, {"1M", 1 << 20}, {"2.4M", 2400 << 10}}

func benchContext(b *testing.B, size int) (*Context, []byte, []byte) {
	b.Helper()
	ctx, err := NewContext(size)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(ctx.Close)
	rng := rand.New(rand.NewSource(1))
	raw := make([]byte, size)
	rng.Read(raw)
	src, dst := ctx.NewBuffer(), ctx.NewBuffer()
	ctx.Prepare(src, raw)
	ctx.Prepare(dst, raw)
	return ctx, dst, src
}

func BenchmarkMulAdd(b *testing.B) {
	for _, s := range benchSizes {
		b.Run(s.name, func(b *testing.B) {
			ctx, dst, src := benchContext(b, s.size)
			b.SetBytes(int64(s.size))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				ctx.MulAdd(dst, src, 0x1234)
			}
		})
	}
}

func BenchmarkMul(b *testing.B) {
	for _, s := range benchSizes[:2] {
		b.Run(s.name, func(b *testing.B) {
			ctx, dst, src := benchContext(b, s.size)
			b.SetBytes(int64(s.size))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				ctx.Mul(dst, src, 0x1234)
			}
		})
	}
}

// BenchmarkMulAddMulti folds n prepared sources per call. Throughput counts
// source bytes, so numbers are comparable across n. "8K" ranges mimic the
// chunked fold workers.
func BenchmarkMulAddMulti(b *testing.B) {
	const size = 64 << 10
	for _, n := range []int{1, 2, 4, 8, 16} {
		for _, chunk := range []int{size, 8 << 10} {
			b.Run(fmt.Sprintf("n=%d/chunk=%dK", n, chunk>>10), func(b *testing.B) {
				ctx, dst, _ := benchContext(b, size)
				rng := rand.New(rand.NewSource(2))
				srcs := make([][]byte, n)
				coeffs := make([]uint16, n)
				raw := make([]byte, size)
				for i := range srcs {
					rng.Read(raw)
					srcs[i] = ctx.NewBuffer()
					ctx.Prepare(srcs[i], raw)
					coeffs[i] = uint16(rng.Intn(1<<16-2)) + 2
				}
				chunk := min(chunk, ctx.BufSize()) / ctx.Stride() * ctx.Stride()
				b.SetBytes(int64(n * size))
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					for off := 0; off+chunk <= ctx.BufSize(); off += chunk {
						ctx.MulAddMulti(dst, srcs, off, chunk, coeffs)
					}
				}
			})
		}
	}
}

// BenchmarkOperations runs batched butterflies over a workspace: adjacent
// rows (the coalesced fast path) and scattered rows.
func BenchmarkOperations(b *testing.B) {
	const size, rows = 64 << 10, 64
	kinds := []struct {
		name string
		kind uint16
	}{{"Add", OpAdd}, {"Scale", OpScale}, {"Butterfly", OpButterfly}, {"InverseButterfly", OpInverseButterfly}}
	for _, k := range kinds {
		for _, layout := range []string{"adjacent", "scattered"} {
			b.Run(k.name+"/"+layout, func(b *testing.B) {
				ctx, err := NewContext(size)
				if err != nil {
					b.Fatal(err)
				}
				set, err := NewBufferWorkspace(ctx, rows)
				if err != nil {
					b.Fatal(err)
				}
				defer set.Close()
				rng := rand.New(rand.NewSource(3))
				for _, row := range set.Buffers() {
					rng.Read(row)
				}
				var ops []Operation
				half := rows / 2
				for r := 0; r < half; r++ {
					d, s := r, half+r
					if layout == "scattered" {
						d, s = 2*r, 2*r+1
					}
					ops = append(ops, Operation{Dst: uint32(d), Src: uint32(s), Coeff: 0x4321, Kind: k.kind})
				}
				if layout == "scattered" {
					// Make the coefficient differ per op so nothing coalesces.
					for i := range ops {
						ops[i].Coeff += uint16(i)
					}
				}
				b.SetBytes(int64(len(ops) * ctx.BufSize()))
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if err := set.Apply(ctx, ops); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// BenchmarkFoldPattern mimics rsec16's streaming fold on one core: 16 prepared
// inputs of a real-sized slice folded into several accumulators in 128K
// ranges, range-major so each range's sources stay cache resident.
func BenchmarkFoldPattern(b *testing.B) {
	const size, batch, outs, rangeBytes = 2380956, 16, 4, 128 << 10
	ctx, _, _ := benchContext(b, size)
	rng := rand.New(rand.NewSource(4))
	raw := make([]byte, size)
	srcs := make([][]byte, batch)
	for i := range srcs {
		rng.Read(raw)
		srcs[i] = ctx.NewBuffer()
		ctx.Prepare(srcs[i], raw)
	}
	accs := ctx.NewBuffers(outs)
	coeffs := make([][]uint16, outs)
	for o := range coeffs {
		coeffs[o] = make([]uint16, batch)
		for j := range coeffs[o] {
			coeffs[o][j] = uint16(rng.Intn(1<<16-1)) + 1
		}
	}
	step := rangeBytes / ctx.Stride() * ctx.Stride()
	b.SetBytes(int64(batch * outs * size))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for off := 0; off < ctx.BufSize(); off += step {
			n := min(step, ctx.BufSize()-off)
			for o := range accs {
				ctx.MulAddMulti(accs[o], srcs, off, n, coeffs[o])
			}
		}
	}
}

// BenchmarkPrepare and BenchmarkFinish cover the layout conversions the fold
// loop runs once per input / accumulator (single-threaded in the reader).
func BenchmarkPrepare(b *testing.B) {
	for _, s := range benchSizes {
		b.Run(s.name, func(b *testing.B) {
			ctx, dst, _ := benchContext(b, s.size)
			raw := make([]byte, s.size)
			rand.New(rand.NewSource(5)).Read(raw)
			b.SetBytes(int64(s.size))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				ctx.Prepare(dst, raw)
			}
		})
	}
}

func BenchmarkFinish(b *testing.B) {
	for _, s := range benchSizes {
		b.Run(s.name, func(b *testing.B) {
			ctx, buf, _ := benchContext(b, s.size)
			out := make([]byte, s.size)
			b.SetBytes(int64(s.size))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				ctx.Finish(buf, out)
			}
		})
	}
}
