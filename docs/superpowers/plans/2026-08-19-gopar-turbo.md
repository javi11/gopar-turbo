# gopar-turbo Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A Go library for PAR2 verify + repair with gopar's API, whose GF(2^16) hot loop runs on ParPar's SIMD kernels via CGO, with a pure-Go fallback.

**Architecture:** Fork of akalin/gopar (`gf2`, `gf2p16`, `rsec16`, `par2`, `memfs` packages). A new public `gf16` package wraps ParPar's `Galois16Mul` C++ class through a thin C bridge (cgo backend) or gopar's `gf2p16` (pure-Go backend, `CGO_ENABLED=0`). `rsec16.Coder.applyMatrix` — the only hot loop, shared by repair and parity generation — dispatches to `gf16` when accelerated.

**Tech Stack:** Go 1.26, cgo, C++11 (ParPar gf16 sources, CC0, vendored from par2go), GNU Make for static libs, testify NOT used (fork uses stdlib testing + `github.com/stretchr/testify` is NOT in gopar — keep stdlib style).

## Global Constraints

- Module path: `github.com/javi11/gopar-turbo`. Go `1.26`.
- ParPar sources come from `/Users/javi/mio/par2go/internal/parpar/vendor/` (CC0). **Never copy code from par2cmdline-turbo (GPLv2).**
- gopar fork source: `/private/tmp/claude-502/-Users-javi-mio/011a6027-9562-4ee3-9605-eda1b923fcb6/scratchpad/gopar` (MIT — keep its LICENSE attribution). If missing, `git clone --depth 1 https://github.com/akalin/gopar <that path>`.
- Public API shapes must not drift from gopar: `par2.Verify(parPath, VerifyOptions)`, `par2.Repair(parPath, RepairOptions)`, `rsec16.Coder`, delegates.
- `gf16.Context` is NOT safe for concurrent `Mul*` calls (C side holds `mutScratch`); `Prepare`/`Finish` are stateless and safe to share. Document on the type; one Context per goroutine for math.
- All commits follow Conventional Commits (`feat:`, `test:`, `chore:`, `docs:`, `ci:`).
- Every task ends with `go test ./...` green (cgo on) before commit. Repo root: `/Users/javi/mio/gopar-turbo`.

---

### Task 1: Fork gopar into the new module

**Files:**
- Create: `go.mod`, `LICENSE`, `LICENSE.gopar`, `gf2/`, `gf2p16/`, `rsec16/`, `par2/`, `memfs/` (copied), `.gitignore`

**Interfaces:**
- Produces: `github.com/javi11/gopar-turbo/{gf2,gf2p16,rsec16,par2,memfs}` — identical APIs to upstream gopar, all upstream tests passing. Later tasks import `gf2p16` (reference math: `gf2p16.T`, `gf2p16.MulByteSliceLE(c T, in, out []byte)`, `gf2p16.MulAndAddByteSliceLE(c T, in, out []byte)`, `gf2p16.Matrix`) and modify `rsec16`.

- [ ] **Step 1: Copy packages and licenses**

```bash
cd /Users/javi/mio/gopar-turbo
SRC=/private/tmp/claude-502/-Users-javi-mio/011a6027-9562-4ee3-9605-eda1b923fcb6/scratchpad/gopar
cp -r $SRC/gf2 $SRC/gf2p16 $SRC/rsec16 $SRC/par2 $SRC/memfs .
cp $SRC/LICENSE LICENSE.gopar
printf 'build/\n*.o\n' > .gitignore
```

Also create `LICENSE` (MIT, copyright 2026 Javier Blanco) — standard MIT text.

- [ ] **Step 2: Rewrite module path**

```bash
cd /Users/javi/mio/gopar-turbo
go mod init github.com/javi11/gopar-turbo
grep -rl 'github.com/akalin/gopar' --include='*.go' . | xargs sed -i '' 's|github.com/akalin/gopar|github.com/javi11/gopar-turbo|g'
go mod tidy
```

Expected deps: `github.com/klauspost/cpuid/v2` (used by `gf2p16/slice_amd64.go`).

- [ ] **Step 3: Run the full forked test suite**

Run: `go test ./...`
Expected: all packages PASS (upstream gopar is green; only the import path changed).

- [ ] **Step 4: Commit**

```bash
git add -A && git commit -m "feat: fork akalin/gopar decode+encode packages as gopar-turbo base"
```

---

### Task 2: Vendor ParPar gf16 kernels, C bridge, static library

**Files:**
- Create: `gf16/vendor/` (copied from par2go), `gf16/csrc/bridge.cpp`, `gf16/bridge.h`, `gf16/Makefile`, `gf16/libgf16_darwin.a` (built artifact, committed)

**Interfaces:**
- Produces: C API in `gf16/bridge.h` (below) linked as `libgf16_<platform>.a`. Task 3 calls it via cgo. Context = one `Galois16Mul` + one `mutScratch`; all `len` arguments must be multiples of `gf16_stride()`.

- [ ] **Step 1: Vendor ParPar sources from par2go**

```bash
cd /Users/javi/mio/gopar-turbo
mkdir -p gf16/csrc
cp -r /Users/javi/mio/par2go/internal/parpar/vendor gf16/vendor
```

(`gf16/vendor/LICENSE` comes along — it is ParPar's CC0 notice. Keep it.)

- [ ] **Step 2: Write `gf16/bridge.h`**

```c
// bridge.h - C API over ParPar's Galois16Mul for cgo.
// A gf16_ctx wraps one Galois16Mul instance plus its mutScratch buffer.
// NOT thread-safe for gf16_mul/gf16_muladd/gf16_muladd_multi (mutScratch);
// gf16_prepare/gf16_finish only touch caller buffers and may run concurrently.
#ifndef GF16_BRIDGE_H
#define GF16_BRIDGE_H

#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

typedef struct gf16_ctx gf16_ctx_t;

// slice_size must be > 0 and even. Returns NULL on failure.
gf16_ctx_t* gf16_create(size_t slice_size);
void        gf16_destroy(gf16_ctx_t* ctx);

const char* gf16_method_name(gf16_ctx_t* ctx);
size_t      gf16_buf_size(gf16_ctx_t* ctx);   // slice_size rounded up to stride
size_t      gf16_alignment(gf16_ctx_t* ctx);  // required buffer alignment
size_t      gf16_stride(gf16_ctx_t* ctx);     // len granularity for mul ops

// Transform src (src_len <= slice_size bytes) into dst (gf16_buf_size bytes,
// aligned). Trailing bytes up to buf_size are zero-padded.
void gf16_prepare(gf16_ctx_t* ctx, void* dst, const void* src, size_t src_len);
// Untransform buf (gf16_buf_size bytes) in place back to standard layout.
void gf16_finish(gf16_ctx_t* ctx, void* buf);

// dst/src are prepared buffers; len is a multiple of stride.
void gf16_mul(gf16_ctx_t* ctx, void* dst, const void* src, size_t len, uint16_t coeff);
void gf16_muladd(gf16_ctx_t* ctx, void* dst, const void* src, size_t len, uint16_t coeff);
// dst += sum(coeffs[i] * srcs[i]) over [offset, offset+len) of each buffer.
void gf16_muladd_multi(gf16_ctx_t* ctx, unsigned n, size_t offset, void* dst,
                       const void* const* srcs, size_t len, const uint16_t* coeffs);

#ifdef __cplusplus
}
#endif
#endif
```

- [ ] **Step 3: Write `gf16/csrc/bridge.cpp`**

```cpp
// bridge.cpp - implements bridge.h over Galois16Mul.
#include "bridge.h"
#include "vendor/gf16/gf16mul.h"

#include <cstring>
#include <new>

struct gf16_ctx {
    Galois16Mul* gf;
    void*        mutScratch;
    size_t       sliceSize;
    size_t       bufSize;
};

extern "C" {

gf16_ctx_t* gf16_create(size_t slice_size) {
    if (slice_size == 0 || (slice_size & 1)) return nullptr;
    gf16_ctx_t* ctx = new (std::nothrow) gf16_ctx_t;
    if (!ctx) return nullptr;
    // forInvert=true biases method choice toward arbitrary-coefficient use.
    Galois16Methods method =
        Galois16Mul::default_method(slice_size, 32768, 65535, true);
    ctx->gf = new (std::nothrow) Galois16Mul(method);
    if (!ctx->gf) { delete ctx; return nullptr; }
    ctx->mutScratch = ctx->gf->mutScratch_alloc();
    ctx->sliceSize  = slice_size;
    ctx->bufSize    = ctx->gf->alignToStride(slice_size);
    return ctx;
}

void gf16_destroy(gf16_ctx_t* ctx) {
    if (!ctx) return;
    if (ctx->gf) {
        ctx->gf->mutScratch_free(ctx->mutScratch);
        delete ctx->gf;
    }
    delete ctx;
}

const char* gf16_method_name(gf16_ctx_t* ctx) { return ctx->gf->info().name; }
size_t gf16_buf_size(gf16_ctx_t* ctx)  { return ctx->bufSize; }
size_t gf16_alignment(gf16_ctx_t* ctx) { return ctx->gf->info().alignment; }
size_t gf16_stride(gf16_ctx_t* ctx)    { return ctx->gf->info().stride; }

void gf16_prepare(gf16_ctx_t* ctx, void* dst, const void* src, size_t src_len) {
    if (src_len < ctx->bufSize)
        memset((uint8_t*)dst + src_len, 0, ctx->bufSize - src_len);
    ctx->gf->prepare(dst, src, src_len);
}

void gf16_finish(gf16_ctx_t* ctx, void* buf) {
    ctx->gf->finish(buf, ctx->bufSize);
}

void gf16_mul(gf16_ctx_t* ctx, void* dst, const void* src, size_t len, uint16_t coeff) {
    ctx->gf->mul(dst, src, len, coeff, ctx->mutScratch);
}

void gf16_muladd(gf16_ctx_t* ctx, void* dst, const void* src, size_t len, uint16_t coeff) {
    ctx->gf->mul_add(dst, src, len, coeff, ctx->mutScratch);
}

void gf16_muladd_multi(gf16_ctx_t* ctx, unsigned n, size_t offset, void* dst,
                       const void* const* srcs, size_t len, const uint16_t* coeffs) {
    ctx->gf->mul_add_multi(n, offset, dst, srcs, len, coeffs, ctx->mutScratch);
}

} // extern "C"
```

- [ ] **Step 4: Write `gf16/Makefile`**

Copy `/Users/javi/mio/par2go/internal/parpar/Makefile` and apply exactly these changes (the SIMD per-file rules stay identical):

1. Replace every occurrence of `libparpar_gf16` with `libgf16` (target names and platform copies).
2. In `CXX_SRCS` and `CXX_OBJS`, drop `controller.cpp` and `controller_cpu.cpp` (the bridge uses `Galois16Mul` directly); keep `gf16mul.cpp` and `csrc/bridge.cpp`.
3. Delete the two Makefile rules for `$(BUILDDIR)/controller.cpp.o` and `$(BUILDDIR)/controller_cpu.cpp.o`.
4. Keep `DEFINES := -DPARPAR_INVERT_SUPPORT -DPARPAR_SLIM_GF16` (INVERT_SUPPORT gates `mul`/`mul_add`/`prepare`/`finish`).

```bash
cp /Users/javi/mio/par2go/internal/parpar/Makefile /Users/javi/mio/gopar-turbo/gf16/Makefile
# then apply edits 1-4 above
```

- [ ] **Step 5: Build the darwin universal library**

Run: `make -C /Users/javi/mio/gopar-turbo/gf16 darwin`
Expected: `gf16/libgf16_darwin.a` exists; `lipo -info gf16/libgf16_darwin.a` reports `x86_64 arm64`.

- [ ] **Step 6: Commit**

```bash
cd /Users/javi/mio/gopar-turbo
git add gf16 && git commit -m "feat(gf16): vendor ParPar kernels, add C bridge and darwin static lib"
```

---

### Task 3: gf16 cgo Go wrapper (TDD against gf2p16 reference)

**Files:**
- Create: `gf16/gf16.go` (shared), `gf16/gf16_cgo.go`, `gf16/gf16_test.go`

**Interfaces:**
- Consumes: C API from Task 2; `gf2p16.MulByteSliceLE`/`MulAndAddByteSliceLE` as test reference.
- Produces (used by Tasks 4-5 and external consumers like altmount):

```go
package gf16
func Accelerated() bool                       // true on cgo backend
func NewContext(sliceSize int) (*Context, error)
func (c *Context) MethodName() string
func (c *Context) BufSize() int               // prepared-buffer size
func (c *Context) Stride() int                // len granularity for Mul ops
func (c *Context) NewBuffer() []byte          // aligned, BufSize() bytes
func (c *Context) Prepare(dst, src []byte)    // len(dst)==BufSize, len(src)<=sliceSize
func (c *Context) Finish(buf, out []byte)     // untransform buf, copy len(out) bytes out
func (c *Context) Mul(dst, src []byte, coeff uint16)      // whole prepared buffers
func (c *Context) MulAdd(dst, src []byte, coeff uint16)
func (c *Context) MulAddMulti(dst []byte, srcs [][]byte, offset, length int, coeffs []uint16)
func (c *Context) Close()
```

- [ ] **Step 1: Write the failing tests (`gf16/gf16_test.go`)**

These tests are backend-agnostic (no build tag) — they validate whichever backend compiles in, always against the pure-Go `gf2p16` reference.

```go
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
		// Finish mutates dstBuf in place; re-run needs no restore since Mul overwrites.
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./gf16/`
Expected: FAIL — `NewContext` undefined (package doesn't compile yet).

- [ ] **Step 3: Write `gf16/gf16.go` (shared, no build tag)**

```go
// Package gf16 provides GF(2^16) region multiply operations for PAR2
// Reed-Solomon coding. With cgo enabled it uses ParPar's SIMD kernels
// (SSE2..AVX-512/GFNI on amd64, NEON/SVE2 on arm64); otherwise it falls
// back to the pure-Go gf2p16 implementation.
//
// Usage pattern (streaming fold or matrix apply):
//
//	ctx, _ := gf16.NewContext(sliceSize)
//	defer ctx.Close()
//	buf := ctx.NewBuffer()
//	ctx.Prepare(buf, slice)           // once per input
//	ctx.MulAdd(acc, buf, coeff)       // any number of times
//	ctx.Finish(acc, out)              // once per output, at the end
//
// A Context is NOT safe for concurrent Mul/MulAdd/MulAddMulti calls;
// use one Context per goroutine. Prepare and Finish are stateless and
// may be called concurrently on distinct buffers.
package gf16

import (
	"errors"
	"unsafe"
)

var errBadSliceSize = errors.New("gf16: slice size must be positive and even")

// alignedSlice returns a length-size slice whose backing array start is
// aligned to align (a power of two).
func alignedSlice(size, align int) []byte {
	buf := make([]byte, size+align)
	off := int(uintptr(unsafe.Pointer(&buf[0])) & uintptr(align-1))
	if off != 0 {
		off = align - off
	}
	return buf[off : off+size : off+size]
}

func checkSliceSize(sliceSize int) error {
	if sliceSize <= 0 || sliceSize%2 != 0 {
		return errBadSliceSize
	}
	return nil
}
```

- [ ] **Step 4: Write `gf16/gf16_cgo.go`**

```go
//go:build cgo && (darwin || (linux && (amd64 || arm64)) || (windows && amd64))

package gf16

/*
#cgo CXXFLAGS: -std=c++11
#cgo darwin LDFLAGS: ${SRCDIR}/libgf16_darwin.a -lstdc++ -lm
#cgo linux,amd64 LDFLAGS: ${SRCDIR}/libgf16_linux_amd64.a -lstdc++ -lm
#cgo linux,arm64 LDFLAGS: ${SRCDIR}/libgf16_linux_arm64.a -lstdc++ -lm
#cgo windows,amd64 LDFLAGS: ${SRCDIR}/libgf16_windows_amd64.a -lstdc++ -lm
#include "bridge.h"
#include <stdlib.h>
*/
import "C"

import (
	"errors"
	"runtime"
	"unsafe"
)

// Accelerated reports whether the SIMD (cgo) backend is in use.
func Accelerated() bool { return true }

// Context wraps one ParPar Galois16Mul instance. Not safe for concurrent
// Mul/MulAdd/MulAddMulti; Prepare/Finish are safe to call concurrently.
type Context struct {
	ctx       *C.gf16_ctx_t
	sliceSize int
	bufSize   int
	stride    int
	align     int
}

func NewContext(sliceSize int) (*Context, error) {
	if err := checkSliceSize(sliceSize); err != nil {
		return nil, err
	}
	ctx := C.gf16_create(C.size_t(sliceSize))
	if ctx == nil {
		return nil, errors.New("gf16: failed to initialize ParPar context")
	}
	c := &Context{
		ctx:       ctx,
		sliceSize: sliceSize,
		bufSize:   int(C.gf16_buf_size(ctx)),
		stride:    int(C.gf16_stride(ctx)),
		align:     int(C.gf16_alignment(ctx)),
	}
	runtime.SetFinalizer(c, (*Context).Close)
	return c, nil
}

func (c *Context) MethodName() string { return C.GoString(C.gf16_method_name(c.ctx)) }
func (c *Context) BufSize() int       { return c.bufSize }
func (c *Context) Stride() int        { return c.stride }

func (c *Context) NewBuffer() []byte { return alignedSlice(c.bufSize, c.align) }

func (c *Context) Prepare(dst, src []byte) {
	if len(dst) != c.bufSize || len(src) > c.sliceSize || len(src) == 0 {
		panic("gf16: Prepare buffer size mismatch")
	}
	C.gf16_prepare(c.ctx, unsafe.Pointer(&dst[0]), unsafe.Pointer(&src[0]), C.size_t(len(src)))
}

func (c *Context) Finish(buf, out []byte) {
	if len(buf) != c.bufSize || len(out) > c.sliceSize {
		panic("gf16: Finish buffer size mismatch")
	}
	C.gf16_finish(c.ctx, unsafe.Pointer(&buf[0]))
	copy(out, buf)
}

func (c *Context) Mul(dst, src []byte, coeff uint16) {
	c.checkPair(dst, src)
	C.gf16_mul(c.ctx, unsafe.Pointer(&dst[0]), unsafe.Pointer(&src[0]),
		C.size_t(len(src)), C.uint16_t(coeff))
}

func (c *Context) MulAdd(dst, src []byte, coeff uint16) {
	c.checkPair(dst, src)
	C.gf16_muladd(c.ctx, unsafe.Pointer(&dst[0]), unsafe.Pointer(&src[0]),
		C.size_t(len(src)), C.uint16_t(coeff))
}

// MulAddMulti computes dst[offset:offset+length] ^= coeffs[i]*srcs[i][offset:...]
// for all i. offset and length must be multiples of Stride().
func (c *Context) MulAddMulti(dst []byte, srcs [][]byte, offset, length int, coeffs []uint16) {
	if len(srcs) == 0 {
		return
	}
	if len(srcs) != len(coeffs) {
		panic("gf16: srcs/coeffs length mismatch")
	}
	if offset%c.stride != 0 || length%c.stride != 0 || offset+length > c.bufSize {
		panic("gf16: MulAddMulti range not stride-aligned or out of bounds")
	}
	ptrs := make([]unsafe.Pointer, len(srcs))
	for i, s := range srcs {
		if len(s) != c.bufSize {
			panic("gf16: MulAddMulti src size mismatch")
		}
		ptrs[i] = unsafe.Pointer(&s[0])
	}
	C.gf16_muladd_multi(c.ctx, C.unsigned(len(srcs)), C.size_t(offset),
		unsafe.Pointer(&dst[0]), (*unsafe.Pointer)(&ptrs[0]),
		C.size_t(length), (*C.uint16_t)(&coeffs[0]))
	runtime.KeepAlive(srcs)
}

func (c *Context) checkPair(dst, src []byte) {
	if len(dst) != c.bufSize || len(src) != c.bufSize {
		panic("gf16: buffer size mismatch")
	}
	if len(src)%c.stride != 0 {
		panic("gf16: length not a stride multiple")
	}
}

func (c *Context) Close() {
	if c.ctx != nil {
		runtime.SetFinalizer(c, nil)
		C.gf16_destroy(c.ctx)
		c.ctx = nil
	}
}
```

Note for the implementer: if the cgo type of `gf16_muladd_multi`'s `srcs`
parameter (`const void* const*`) fights Go's `*unsafe.Pointer`, add a small
adapter in `bridge.h` taking `void**` instead — do NOT fight cgo casts.

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./gf16/ -v`
Expected: all `TestPrepare*/TestMul*` PASS. Also run `go test ./gf16/ -run TestMulMatchesReference -count=5` for flake check.

- [ ] **Step 6: Commit**

```bash
git add gf16 && git commit -m "feat(gf16): cgo wrapper over ParPar Galois16Mul with reference-checked tests"
```

---

### Task 4: gf16 pure-Go fallback backend

**Files:**
- Create: `gf16/gf16_purego.go`

**Interfaces:**
- Consumes: `gf2p16.MulByteSliceLE`, `gf2p16.MulAndAddByteSliceLE`.
- Produces: identical `gf16` API with `Accelerated() == false`. Same tests must pass with `CGO_ENABLED=0`.

- [ ] **Step 1: Verify the failing state**

Run: `CGO_ENABLED=0 go test ./gf16/`
Expected: FAIL — no backend compiles under `!cgo` (build error).

- [ ] **Step 2: Write `gf16/gf16_purego.go`**

```go
//go:build !cgo || !(darwin || (linux && (amd64 || arm64)) || (windows && amd64))

package gf16

import "github.com/javi11/gopar-turbo/gf2p16"

// Accelerated reports whether the SIMD (cgo) backend is in use.
func Accelerated() bool { return false }

// Context is the pure-Go fallback backend. The prepared layout is the
// identity (a plain copy), stride is 2 bytes (one GF(2^16) word).
type Context struct {
	sliceSize int
	bufSize   int
}

func NewContext(sliceSize int) (*Context, error) {
	if err := checkSliceSize(sliceSize); err != nil {
		return nil, err
	}
	return &Context{sliceSize: sliceSize, bufSize: sliceSize}, nil
}

func (c *Context) MethodName() string { return "gf2p16 (pure Go)" }
func (c *Context) BufSize() int       { return c.bufSize }
func (c *Context) Stride() int        { return 2 }
func (c *Context) NewBuffer() []byte  { return alignedSlice(c.bufSize, 16) }

func (c *Context) Prepare(dst, src []byte) {
	if len(dst) != c.bufSize || len(src) > c.sliceSize || len(src) == 0 {
		panic("gf16: Prepare buffer size mismatch")
	}
	n := copy(dst, src)
	for i := n; i < len(dst); i++ {
		dst[i] = 0
	}
}

func (c *Context) Finish(buf, out []byte) {
	if len(buf) != c.bufSize || len(out) > c.sliceSize {
		panic("gf16: Finish buffer size mismatch")
	}
	copy(out, buf)
}

func (c *Context) Mul(dst, src []byte, coeff uint16) {
	gf2p16.MulByteSliceLE(gf2p16.T(coeff), src, dst)
}

func (c *Context) MulAdd(dst, src []byte, coeff uint16) {
	gf2p16.MulAndAddByteSliceLE(gf2p16.T(coeff), src, dst)
}

func (c *Context) MulAddMulti(dst []byte, srcs [][]byte, offset, length int, coeffs []uint16) {
	if len(srcs) != len(coeffs) {
		panic("gf16: srcs/coeffs length mismatch")
	}
	for i, s := range srcs {
		gf2p16.MulAndAddByteSliceLE(gf2p16.T(coeffs[i]),
			s[offset:offset+length], dst[offset:offset+length])
	}
}

func (c *Context) Close() {}
```

- [ ] **Step 3: Run tests under both backends**

Run: `CGO_ENABLED=0 go test ./gf16/ && go test ./gf16/`
Expected: PASS twice (purego then cgo).

- [ ] **Step 4: Commit**

```bash
git add gf16 && git commit -m "feat(gf16): pure-Go fallback backend for CGO_ENABLED=0"
```

---

### Task 5: rsec16 dispatches applyMatrix through gf16

**Files:**
- Create: `rsec16/apply_gf16.go`, `rsec16/apply_gf16_test.go`
- Modify: `rsec16/coder.go:141-143` (`applyMatrix`)

**Interfaces:**
- Consumes: `gf16.NewContext`, `Prepare`, `MulAddMulti`, `Finish`, `Accelerated`; `gf2p16.Matrix.At(i, j) gf2p16.T`; existing `applyMatrixParallelData(m, in, out, numGoroutines)` and `calculateParallelParams(totalLength, numGoroutines, minPerGoroutineLength, perGoroutineLengthDivisor int) (perGoroutineLength, newNumGoroutines int)` from `rsec16/matrix.go`.
- Produces: `applyMatrixGF16(m gf2p16.Matrix, in, out [][]byte, numGoroutines int)` — same semantics as `applyMatrixParallelData` (out rows fully overwritten). `Coder.GenerateParity` and `Coder.ReconstructData` transparently accelerate — no API change.

- [ ] **Step 1: Write the failing test (`rsec16/apply_gf16_test.go`)**

```go
package rsec16

import (
	"bytes"
	"math/rand"
	"testing"

	"github.com/javi11/gopar-turbo/gf16"
	"github.com/javi11/gopar-turbo/gf2p16"
)

func TestApplyMatrixGF16MatchesReference(t *testing.T) {
	rng := rand.New(rand.NewSource(6))
	for _, tc := range []struct {
		name                       string
		inShards, outShards, size int
		goroutines                 int
	}{
		{"small", 3, 2, 8, 1},
		{"typical", 13, 5, 4096, 4},
		{"odd-slice", 7, 3, 4096 + 2, 4},
		{"more-goroutines-than-rows", 5, 2, 65536, 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := gf2p16.NewMatrixFromFunction(tc.outShards, tc.inShards,
				func(i, j int) gf2p16.T { return gf2p16.T(rng.Intn(1 << 16)) })
			in := make([][]byte, tc.inShards)
			for i := range in {
				in[i] = make([]byte, tc.size)
				rng.Read(in[i])
			}
			want := make([][]byte, tc.outShards)
			got := make([][]byte, tc.outShards)
			for i := range want {
				want[i] = make([]byte, tc.size)
				got[i] = make([]byte, tc.size)
			}
			applyMatrixParallelData(m, in, want, tc.goroutines)
			applyMatrixGF16(m, in, got, tc.goroutines)
			for i := range want {
				if !bytes.Equal(want[i], got[i]) {
					t.Errorf("output shard %d differs", i)
				}
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./rsec16/ -run TestApplyMatrixGF16`
Expected: FAIL — `applyMatrixGF16` undefined.

- [ ] **Step 3: Write `rsec16/apply_gf16.go`**

```go
package rsec16

import (
	"sync"

	"github.com/javi11/gopar-turbo/gf16"
	"github.com/javi11/gopar-turbo/gf2p16"
)

// applyMatrixGF16 computes out[i] = sum_j m[i][j]*in[j] using the gf16
// backend. Semantics match applyMatrixParallelData: every out row is fully
// overwritten. in rows must all have len(in[0]) bytes; out rows likewise.
func applyMatrixGF16(m gf2p16.Matrix, in, out [][]byte, numGoroutines int) {
	sliceSize := len(in[0])
	// One context for layout metadata and prepares (Prepare is stateless).
	ctx, err := gf16.NewContext(sliceSize)
	if err != nil {
		// Sizes are validated by callers; treat as programmer error like
		// the rest of this package does.
		panic(err)
	}
	defer ctx.Close()

	// Prepare all inputs once, in parallel.
	prepared := make([][]byte, len(in))
	var wg sync.WaitGroup
	wg.Add(len(in))
	for j := range in {
		go func(j int) {
			defer wg.Done()
			prepared[j] = ctx.NewBuffer()
			ctx.Prepare(prepared[j], in[j])
		}(j)
	}
	wg.Wait()

	// Zero-prepared accumulators for each output row.
	accs := make([][]byte, len(out))
	zero := make([]byte, sliceSize)
	for i := range out {
		accs[i] = ctx.NewBuffer()
		ctx.Prepare(accs[i], zero)
	}

	// Split work into (output row x stride-aligned data range) units.
	bufSize := ctx.BufSize()
	perRange, numRanges := calculateParallelParams(
		bufSize, (numGoroutines+len(out)-1)/len(out), ctx.Stride(), ctx.Stride())

	type unit struct{ row, offset, length int }
	units := make(chan unit)
	var workers sync.WaitGroup
	if numGoroutines < 1 {
		numGoroutines = 1
	}
	workers.Add(numGoroutines)
	for w := 0; w < numGoroutines; w++ {
		go func() {
			defer workers.Done()
			// Each worker needs its own context: Mul* calls share mutScratch.
			wctx, err := gf16.NewContext(sliceSize)
			if err != nil {
				panic(err)
			}
			defer wctx.Close()
			coeffs := make([]uint16, len(in))
			for u := range units {
				for j := range in {
					coeffs[j] = uint16(m.At(u.row, j))
				}
				wctx.MulAddMulti(accs[u.row], prepared, u.offset, u.length, coeffs)
			}
		}()
	}
	for i := range out {
		for r := 0; r < numRanges; r++ {
			offset := r * perRange
			length := perRange
			if offset+length > bufSize {
				length = bufSize - offset
			}
			if length > 0 {
				units <- unit{row: i, offset: offset, length: length}
			}
		}
	}
	close(units)
	workers.Wait()

	// Untransform accumulators into the caller's out rows, in parallel.
	wg.Add(len(out))
	for i := range out {
		go func(i int) {
			defer wg.Done()
			ctx.Finish(accs[i], out[i])
		}(i)
	}
	wg.Wait()
}
```

**Correctness note for the implementer:** two workers must never write the
same `(row, offset)` range — the unit generation above guarantees disjoint
ranges, and `MulAddMulti` only touches `[offset, offset+length)` of the
accumulator, so concurrent workers on the same row but different ranges are
safe. If a race is suspected, run the test with `-race`.

- [ ] **Step 4: Wire the dispatch in `rsec16/coder.go`**

Replace the body of `applyMatrix` (currently `applyMatrixParallelData(m, in, out, c.numGoroutines)`):

```go
func (c Coder) applyMatrix(m gf2p16.Matrix, in, out [][]byte) {
	if gf16.Accelerated() {
		applyMatrixGF16(m, in, out, c.numGoroutines)
		return
	}
	applyMatrixParallelData(m, in, out, c.numGoroutines)
}
```

Add `"github.com/javi11/gopar-turbo/gf16"` to `rsec16/coder.go` imports.

- [ ] **Step 5: Run all tests, both backends, with race detector**

Run: `go test ./... -race && CGO_ENABLED=0 go test ./...`
Expected: PASS — all upstream rsec16 and par2 tests (GenerateParity, ReconstructData, Verify, Repair) now exercise the gf16 path under cgo and the original path under purego.

- [ ] **Step 6: Commit**

```bash
git add rsec16 && git commit -m "feat(rsec16): route applyMatrix through SIMD gf16 backend when accelerated"
```

---

### Task 6: Real-tool integration fixtures (par2cmdline round-trip)

**Files:**
- Create: `par2/testdata/realtool/` (committed fixtures), `par2/realtool_test.go`

**Interfaces:**
- Consumes: `par2.Verify(parPath string, options VerifyOptions) (VerifyResult, error)`, `par2.Repair(parPath string, options RepairOptions) (RepairResult, error)` — both operate on the process working directory relative to parPath via `defaultFileIO`.

- [ ] **Step 1: Generate fixtures with real par2cmdline (one-time, committed)**

```bash
cd /Users/javi/mio/gopar-turbo
mkdir -p par2/testdata/realtool && cd par2/testdata/realtool
dd if=/dev/urandom of=fileA.bin bs=1024 count=37
dd if=/dev/urandom of=fileB.bin bs=1024 count=11
/opt/homebrew/bin/par2 create -s4096 -c10 testset.par2 fileA.bin fileB.bin
md5 -q fileA.bin fileB.bin > checksums.md5
ls   # expect: fileA.bin fileB.bin testset.par2 testset.vol*.par2 checksums.md5
```

- [ ] **Step 2: Write the failing test (`par2/realtool_test.go`)**

```go
package par2

import (
	"crypto/md5"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// copyRealtoolFixture copies par2/testdata/realtool into a temp dir and
// returns that dir plus the expected md5 (hex) of each data file.
func copyRealtoolFixture(t *testing.T) (dir string, wantMD5 map[string]string) {
	t.Helper()
	dir = t.TempDir()
	entries, err := os.ReadDir("testdata/realtool")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join("testdata/realtool", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, e.Name()), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sums, err := os.ReadFile(filepath.Join(dir, "checksums.md5"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Fields(strings.TrimSpace(string(sums)))
	wantMD5 = map[string]string{"fileA.bin": lines[0], "fileB.bin": lines[1]}
	return dir, wantMD5
}

func fileMD5(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := md5.Sum(data)
	return hex.EncodeToString(sum[:])
}

func TestRealToolVerifyIntact(t *testing.T) {
	dir, _ := copyRealtoolFixture(t)
	result, err := Verify(filepath.Join(dir, "testset.par2"), VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !result.ShardCounts.RepairNeeded() {
		return // intact set: nothing to repair — expected
	}
	t.Error("intact fixture reported as needing repair")
}

func TestRealToolRepairDeletedFile(t *testing.T) {
	dir, wantMD5 := copyRealtoolFixture(t)
	if err := os.Remove(filepath.Join(dir, "fileA.bin")); err != nil {
		t.Fatal(err)
	}
	result, err := Repair(filepath.Join(dir, "testset.par2"), RepairOptions{DoubleCheck: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.RepairedPaths) == 0 {
		t.Fatal("expected repaired paths")
	}
	for name, want := range wantMD5 {
		if got := fileMD5(t, filepath.Join(dir, name)); got != want {
			t.Errorf("%s: md5 = %s, want %s", name, got, want)
		}
	}
}

func TestRealToolRepairCorruptedFile(t *testing.T) {
	dir, wantMD5 := copyRealtoolFixture(t)
	path := filepath.Join(dir, "fileB.bin")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 100; i < 5000; i++ { // stomp across a slice boundary
		data[i] ^= 0xff
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Repair(filepath.Join(dir, "testset.par2"), RepairOptions{DoubleCheck: true}); err != nil {
		t.Fatal(err)
	}
	for name, want := range wantMD5 {
		if got := fileMD5(t, filepath.Join(dir, name)); got != want {
			t.Errorf("%s: md5 = %s, want %s", name, got, want)
		}
	}
}
```

(`ShardCounts.RepairNeeded()` is defined in `par2/decoder.go:604` — it exists in the fork.)

- [ ] **Step 3: Run tests**

Run: `go test ./par2/ -run TestRealTool -v`
Expected: PASS (decoder logic is upstream-proven; this validates our
accelerated pipeline against real-par2cmdline-generated recovery data). If
FAIL, debug with superpowers:systematic-debugging — do not weaken assertions.

- [ ] **Step 4: Run both backends**

Run: `go test ./par2/ -run TestRealTool && CGO_ENABLED=0 go test ./par2/ -run TestRealTool`
Expected: PASS twice.

- [ ] **Step 5: Commit**

```bash
git add par2 && git commit -m "test(par2): round-trip repair against real par2cmdline fixtures"
```

---

### Task 7: Benchmarks, README, CI for prebuilt libraries

**Files:**
- Create: `rsec16/bench_test.go`, `README.md`, `.github/workflows/build-libs.yml`, `.github/workflows/test.yml`

**Interfaces:**
- Consumes: `NewCoderPAR2Vandermonde(dataShards, parityShards, numGoroutines int) (Coder, error)`, `Coder.ReconstructData(data, parity [][]byte) error`, `Coder.GenerateParity(data [][]byte) [][]byte`.

- [ ] **Step 1: Write `rsec16/bench_test.go`**

```go
package rsec16

import (
	"math/rand"
	"runtime"
	"testing"

	"github.com/javi11/gopar-turbo/gf16"
)

// benchReconstruct measures repair throughput: dataShards inputs of
// sliceSize bytes with `missing` of them deleted, reconstructed from parity.
func benchReconstruct(b *testing.B, dataShards, parityShards, missing, sliceSize int) {
	rng := rand.New(rand.NewSource(7))
	c, err := NewCoderPAR2Vandermonde(dataShards, parityShards, runtime.NumCPU())
	if err != nil {
		b.Fatal(err)
	}
	orig := make([][]byte, dataShards)
	for i := range orig {
		orig[i] = make([]byte, sliceSize)
		rng.Read(orig[i])
	}
	parity := c.GenerateParity(orig)
	b.SetBytes(int64(dataShards * sliceSize))
	b.ReportMetric(0, "ns/op") // keep default; SetBytes gives MB/s
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		data := make([][]byte, dataShards)
		for j := range data {
			if j < missing {
				data[j] = nil
			} else {
				data[j] = orig[j]
			}
		}
		b.StartTimer()
		if err := c.ReconstructData(data, parity); err != nil {
			b.Fatal(err)
		}
	}
	b.Logf("backend accelerated=%v", gf16.Accelerated())
}

func BenchmarkReconstruct_100x10_64K(b *testing.B)  { benchReconstruct(b, 100, 10, 10, 65536) }
func BenchmarkReconstruct_1000x50_64K(b *testing.B) { benchReconstruct(b, 1000, 50, 50, 65536) }
func BenchmarkReconstruct_100x10_1M(b *testing.B)   { benchReconstruct(b, 100, 10, 10, 1<<20) }

func BenchmarkGenerateParity_100x10_64K(b *testing.B) {
	rng := rand.New(rand.NewSource(8))
	c, err := NewCoderPAR2Vandermonde(100, 10, runtime.NumCPU())
	if err != nil {
		b.Fatal(err)
	}
	data := make([][]byte, 100)
	for i := range data {
		data[i] = make([]byte, 65536)
		rng.Read(data[i])
	}
	b.SetBytes(int64(100 * 65536))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.GenerateParity(data)
	}
}
```

- [ ] **Step 2: Run and record both backends**

```bash
go test ./rsec16/ -bench . -benchtime 2s | tee /tmp/bench-cgo.txt
CGO_ENABLED=0 go test ./rsec16/ -bench . -benchtime 2s | tee /tmp/bench-purego.txt
```

Expected: cgo MB/s ≥ 5x purego MB/s on the 64K benchmarks (success criterion from the spec). Record both numbers in the README table in Step 3.

- [ ] **Step 3: Write `README.md`**

Contents (prose, ~1 page): what it is (SIMD-accelerated PAR2 verify/repair, gopar-compatible API, companion to par2go for creation); quick-start `Verify`/`Repair` example copied from `par2` doc comments; the `gf16` public package with the streaming-fold example from its package doc (mention altmount-style consumers); backend table (cgo methods vs pure-Go fallback, `CGO_ENABLED=0` support); measured benchmark table from Step 2; licensing paragraph (MIT; gopar MIT attribution in LICENSE.gopar; ParPar CC0 in gf16/vendor/LICENSE; explicitly: no par2cmdline-turbo code); rebuild instructions `make -C gf16 <platform>`.

- [ ] **Step 4: Write `.github/workflows/build-libs.yml`**

```yaml
name: build-libs
on: workflow_dispatch
jobs:
  darwin:
    runs-on: macos-latest
    steps:
      - uses: actions/checkout@v4
      - run: make -C gf16 darwin
      - uses: actions/upload-artifact@v4
        with: { name: libgf16_darwin, path: gf16/libgf16_darwin.a }
  linux-amd64:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: make -C gf16 linux/amd64
      - uses: actions/upload-artifact@v4
        with: { name: libgf16_linux_amd64, path: gf16/libgf16_linux_amd64.a }
  linux-arm64:
    runs-on: ubuntu-24.04-arm
    steps:
      - uses: actions/checkout@v4
      - run: make -C gf16 linux/arm64
      - uses: actions/upload-artifact@v4
        with: { name: libgf16_linux_arm64, path: gf16/libgf16_linux_arm64.a }
  windows-amd64:
    runs-on: windows-latest
    defaults: { run: { shell: msys2 {0} } }
    steps:
      - uses: actions/checkout@v4
      - uses: msys2/setup-msys2@v2
        with: { msystem: MINGW64, install: "mingw-w64-x86_64-gcc make" }
      - run: make -C gf16 windows/amd64
      - uses: actions/upload-artifact@v4
        with: { name: libgf16_windows_amd64, path: gf16/libgf16_windows_amd64.a }
```

- [ ] **Step 5: Write `.github/workflows/test.yml`**

```yaml
name: test
on: [push, pull_request]
jobs:
  test:
    strategy:
      matrix:
        include:
          - { os: macos-latest, cgo: "1" }
          - { os: macos-latest, cgo: "0" }
          - { os: ubuntu-latest, cgo: "0" }
    runs-on: ${{ matrix.os }}
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version: "1.26" }
      - run: CGO_ENABLED=${{ matrix.cgo }} go test ./... -race
        if: matrix.cgo == '1'
      - run: CGO_ENABLED=${{ matrix.cgo }} go test ./...
        if: matrix.cgo == '0'
```

(ubuntu cgo=1 joins the matrix once `libgf16_linux_amd64.a` from build-libs
is downloaded and committed — note this in the README.)

- [ ] **Step 6: Final full check and commit**

Run: `go test ./... -race && CGO_ENABLED=0 go test ./... && go vet ./...`
Expected: PASS, no vet issues in new files (forked gopar files keep upstream style).

```bash
git add -A && git commit -m "feat: benchmarks, README, CI for prebuilt gf16 libraries"
```
