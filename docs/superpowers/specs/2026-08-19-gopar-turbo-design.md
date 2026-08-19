# gopar-turbo — SIMD-accelerated PAR2 verify & repair for Go

**Date:** 2026-08-19
**Status:** Approved

## Goal

A Go library for PAR2 **verify and repair** with a gopar-style programmatic API
(custom I/O, delegates), accelerated by ParPar's SIMD GF(2^16) kernels via CGO.
It is the repair-side companion to [par2go](https://github.com/javi11/par2go)
(which handles creation). Target: 5–20x+ repair throughput over upstream
[akalin/gopar](https://github.com/akalin/gopar).

## Scope

- **In:** PAR2 verify + repair, custom I/O (`fileIO` interface), progress
  delegates, pure-Go fallback when CGO is disabled.
- **Out:** PAR2 creation (par2go covers it), PAR1, streaming/chunked I/O
  (v1 keeps gopar's whole-file-in-memory model), Windows arm64.

## Licensing

par2cmdline-turbo is GPLv2 — **no code is copied from it**; it serves only as a
reference for driving GF16 kernels in a repair context. The SIMD kernels are
vendored from **ParPar (CC0/public domain)** — the same kernels
par2cmdline-turbo uses, same author (animetosho). The library stays
MIT-compatible. gopar is MIT; forked portions retain its license attribution.

## Architecture

Fork of akalin/gopar stripped to the decode path, with the math backend made
pluggable:

```
gopar-turbo/                    module github.com/javi11/gopar-turbo
├── par2/         forked: packets, Decoder, Verify, Repair, DecoderDelegate,
│                 fileIO interface (custom I/O preserved). Encode path deleted.
├── rsec16/       forked: RS coder, reconstruction matrix construction + inversion
├── gf2p16/       forked: pure-Go GF(2^16) field math (matrix inversion + fallback)
├── gf16/         NEW: hot-loop backend, two implementations behind one Go API:
│   ├── gf16_cgo.go      build tag cgo: ParPar kernels via C bridge
│   ├── gf16_purego.go   build tag !cgo: wraps gf2p16 (SSSE3/scalar)
│   ├── bridge.h / bridge.cpp   thin C bridge over ParPar's Galois16Mul
│   ├── vendor/          ParPar gf16 sources (CC0)
│   └── libgf16_*.a      prebuilt static libs per platform
└── cmd/par2repair/      optional CLI for manual testing
```

`gf16` is a **public** package with a documented Go API (`NewContext`, `Prepare`,
`Mul`, `MulAdd`, `MulAddMulti`, `Finish`, `Close`) so external consumers — e.g.
altmount's streaming PAR2 solver (javi11/altmount#829), which folds slices into
accumulators using gopar's `gf2p16` today — can use the SIMD backend directly
without depending on the Decoder layer. Accumulator-style streaming use is a
first-class scenario: prepare each input once, muladd into k resident
accumulators, finish only at the end.

Layering: `par2` → `rsec16` → `gf16` (hot loop) / `gf2p16` (scalar math).

## CGO bridge API

New thin bridge directly over `Galois16Mul` (par2go's existing bridge wraps the
encode-only controller and is not reused):

```c
gf16_ctx*   gf16_create(size_t slice_size);   // runtime CPU detection, best method
const char* gf16_method_name(gf16_ctx*);
size_t      gf16_alignment(gf16_ctx*);
size_t      gf16_stride(gf16_ctx*);
void gf16_prepare(gf16_ctx*, void* dst, const void* src, size_t len);  // → ALTMAP
void gf16_mul   (gf16_ctx*, void* dst, const void* src, size_t len, uint16_t coeff);
void gf16_muladd(gf16_ctx*, void* dst, const void* src, size_t len, uint16_t coeff);
void gf16_muladd_multi(gf16_ctx*, void* dst, const void** srcs,
                       const uint16_t* coeffs, int n, size_t len);  // batched
void gf16_finish(gf16_ctx*, void* buf, size_t len);  // ALTMAP → standard
void gf16_destroy(gf16_ctx*);
```

Errors surface as NULL/int codes at the C boundary and are wrapped into Go
errors in `gf16_cgo.go`.

## Repair data flow

1. Parse packets + verify shards (pure Go, unchanged from gopar: CRC32/MD5
   shard location map).
2. Build reconstruction matrix and invert in `gf2p16` (matrices are tiny —
   scalar Go is fine).
3. `gf16_prepare` all available shards once into aligned buffers.
4. For each missing shard: `gf16_muladd_multi` sweeps over input batches
   (one large CGO call per batch — call overhead is amortized).
5. `gf16_finish` reconstructed shards; write via `fileIO`.

## Concurrency & memory

- `numGoroutines` option, same as gopar. Work split by output-shard ranges.
- One `gf16_ctx` per goroutine (ParPar contexts are not thread-safe; creation
  is cheap).
- Buffers allocated Go-side, padded to `gf16_alignment()`/`gf16_stride()`.
- Whole-file-in-memory (gopar `fileIO.ReadFile` semantics) in v1.

## Platforms & fallback

- **CGO on:** SSE2/SSSE3/AVX2/AVX-512/GFNI (amd64), NEON/SVE2 (arm64).
  Prebuilt static libs committed for darwin/{amd64,arm64},
  linux/{amd64,arm64}, windows/amd64 — same Makefile pattern as
  par2go/internal/parpar.
- **CGO off / other platforms:** gopar's original pure-Go + SSSE3 path via
  build tags. Identical results, identical API.

## Testing

1. **Property tests:** cgo backend vs pure-Go `gf2p16` reference — random
   coefficients/lengths/data must match bit-for-bit.
2. **Round-trip integration:** create PAR2 sets with par2go and par2cmdline,
   corrupt/delete slices in controlled patterns, repair, verify MD5s. Keep
   gopar's existing test corpus green.
3. **Fallback parity:** same repair with both backends → identical outputs.
4. **Benchmarks:** repair throughput vs upstream gopar, tracked in `benches/`.

## Success criteria

- Drop-in-shaped API for gopar users (Decoder / Verify / Repair / delegates).
- ≥5x repair throughput vs upstream gopar on AVX2 hardware.
- `CGO_ENABLED=0` builds work everywhere Go runs.
- Output byte-identical to par2cmdline repair results on the test corpus.
