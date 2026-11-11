//go:build unix

package gf16

import (
	"bytes"
	"math/rand"
	"os"
	"syscall"
	"testing"

	"github.com/javi11/gopar-turbo/gf2p16"
)

// Guard-page tests: every buffer handed to a kernel sits flush against a
// PROT_NONE page (directly after, or directly before), so any out-of-bounds
// access crashes the test instead of silently corrupting memory.

type guardArena struct {
	t    *testing.T
	maps [][]byte
}

func newGuardArena(t *testing.T) *guardArena {
	a := &guardArena{t: t}
	t.Cleanup(a.release)
	return a
}

func (a *guardArena) release() {
	for _, m := range a.maps {
		_ = syscall.Munmap(m)
	}
	a.maps = nil
}

// alloc returns an n-byte slice whose last byte is the last byte before a
// guard page (end) or whose first byte is the first after one (!end).
func (a *guardArena) alloc(n int, end bool) []byte {
	a.t.Helper()
	pg := os.Getpagesize()
	body := (n + pg - 1) / pg * pg
	mem, err := syscall.Mmap(-1, 0, body+2*pg, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_PRIVATE)
	if err != nil {
		a.t.Fatal(err)
	}
	if err := syscall.Mprotect(mem[:pg], syscall.PROT_NONE); err != nil {
		a.t.Fatal(err)
	}
	if err := syscall.Mprotect(mem[pg+body:], syscall.PROT_NONE); err != nil {
		a.t.Fatal(err)
	}
	a.maps = append(a.maps, mem)
	if end {
		return mem[pg+body-n : pg+body : pg+body]
	}
	return mem[pg : pg+n : pg+n]
}

func TestGuardPageKernels(t *testing.T) {
	for _, k := range kernelVariants {
		t.Run(k.name, func(t *testing.T) {
			rng := rand.New(rand.NewSource(1))
			arena := newGuardArena(t)
			for _, end := range []bool{true, false} {
				for blocks := 1; blocks <= 10; blocks++ {
					n := blocks * k.stride
					srcBufs := make([][]byte, maxPassRegions)
					var srcs srcPtrs
					var cs coefList
					for i := range srcBufs {
						srcBufs[i] = arena.alloc(n, end)
						rng.Read(srcBufs[i])
						srcs[i] = &srcBufs[i][0]
						cs[i] = uint16(rng.Intn(65535) + 1)
					}
					dst := arena.alloc(n, end)
					plain := func(buf []byte) []byte {
						out := make([]byte, n)
						k.finish(&out[0], &buf[0], n)
						return out
					}
					for nsrc := 1; nsrc <= maxPassRegions; nsrc++ {
						rng.Read(dst)
						want := plain(dst)
						for i := 0; i < nsrc; i++ {
							gf2p16.MulAndAddByteSliceLE(gf2p16.T(cs[i]), plain(srcBufs[i]), want)
						}
						k.mulAddMulti(&dst[0], &srcs, &cs, nsrc, n)
						if !bytes.Equal(plain(dst), want) {
							t.Fatalf("mulAddMulti end=%v n=%d nsrc=%d mismatch", end, n, nsrc)
						}
					}
					// mul, mulAdd, xor, prepare, finish on the same buffers.
					c := cs[0]
					want := make([]byte, n)
					gf2p16.MulByteSliceLE(gf2p16.T(c), plain(srcBufs[0]), want)
					rng.Read(dst)
					k.mul(&dst[0], srcs[0], n, c)
					if !bytes.Equal(plain(dst), want) {
						t.Fatalf("mul end=%v n=%d mismatch", end, n)
					}
					want = plain(dst)
					gf2p16.MulAndAddByteSliceLE(gf2p16.T(c), plain(srcBufs[1]), want)
					k.mulAdd(&dst[0], srcs[1], n, c)
					if !bytes.Equal(plain(dst), want) {
						t.Fatalf("mulAdd end=%v n=%d mismatch", end, n)
					}
					want = plain(dst)
					for i, b := range plain(srcBufs[2]) {
						want[i] ^= b
					}
					k.xor(&dst[0], srcs[2], n)
					if !bytes.Equal(plain(dst), want) {
						t.Fatalf("xor end=%v n=%d mismatch", end, n)
					}
					orig := arena.alloc(n, end)
					rng.Read(orig)
					k.prepare(&dst[0], &orig[0], n)
					if !bytes.Equal(plain(dst), orig) {
						t.Fatalf("prepare/finish end=%v n=%d round trip mismatch", end, n)
					}
				}
				arena.release()
			}
		})
	}
}

// TestGuardPageContext runs the Context glue (Prepare, Finish, Mul, MulAdd,
// MulAddMulti) on guarded buffers for every even slice size up to 200.
func TestGuardPageContext(t *testing.T) {
	forEachPath(t, func(t *testing.T, k *kernel) {
		rng := rand.New(rand.NewSource(3))
		arena := newGuardArena(t)
		for _, end := range []bool{true, false} {
			for size := 2; size <= 200; size += 2 {
				ctx := mustContext(t, size, k)
				src := arena.alloc(size, end)
				rng.Read(src)
				a := arena.alloc(ctx.BufSize(), end)
				b := arena.alloc(ctx.BufSize(), end)
				out := arena.alloc(size, end)
				rng.Read(a)
				ctx.Prepare(a, src)
				ctx.Finish(a, out)
				if !bytes.Equal(out, src) {
					t.Fatalf("size=%d end=%v: Prepare/Finish round trip", size, end)
				}
				rng.Read(b)
				for _, c := range []uint16{0, 1, 2, 0xffff, 0x1234} {
					ctx.Mul(a, b, c)
					ctx.MulAdd(a, b, c)
					ctx.Mul(a, a, c)
					ctx.MulAdd(a, a, c)
					ctx.MulAddMulti(a, [][]byte{b, b, a}, 0, ctx.BufSize(), []uint16{c, 3, c})
				}
				arena.release()
			}
		}
	})
}
