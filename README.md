# gopar-turbo

SIMD-accelerated **PAR2 verify and repair** for Go, with a
[gopar](https://github.com/akalin/gopar)-compatible API. The GF(2^16)
Reed-Solomon hot loop runs on [ParPar](https://github.com/animetosho/ParPar)'s
SIMD kernels (SSE2 … AVX-512/GFNI on amd64, NEON/SVE2 on arm64) via cgo, with
runtime CPU detection and a pure-Go fallback for `CGO_ENABLED=0` builds.

It is the repair-side companion to [par2go](https://github.com/javi11/par2go):
par2go creates PAR2 sets, gopar-turbo verifies and repairs them. (An encode
path exists because the fork inherits it — and it accelerates for free — but
creation is par2go's job and is not a supported surface here.)

## Install

```bash
go get github.com/javi11/gopar-turbo
```

Requires Go 1.26+. With cgo enabled (the default), prebuilt static libraries
are linked automatically on darwin (universal), linux/amd64, linux/arm64, and
windows/amd64 (MinGW required on Windows). Everywhere else — or with
`CGO_ENABLED=0` — the pure-Go backend is used, with identical results.

## Verify and repair

```go
import "github.com/javi11/gopar-turbo/par2"

result, err := par2.Verify("recovery.par2", par2.VerifyOptions{})
// result.ShardCounts.RepairNeeded(), result.ShardCounts.RepairPossible()

repaired, err := par2.Repair("recovery.par2", par2.RepairOptions{DoubleCheck: true})
// repaired.RepairedPaths
```

The API is shaped like gopar's: delegates for progress
(`VerifyDelegate`, `RepairDelegate`), `NumGoroutines` options, and the same
error semantics (`par2.RepairErrorMeansRepairNecessaryButNotPossible`).

## The gf16 package (streaming consumers)

`github.com/javi11/gopar-turbo/gf16` exposes the SIMD backend directly for
external solvers that fold slices into accumulators (e.g. streaming PAR2
repair over NNTP, where each present slice is folded into k resident
accumulators and discarded):

```go
ctx, _ := gf16.NewContext(sliceSize)
defer ctx.Close()

acc := ctx.NewBuffer()
ctx.Prepare(acc, zeros)          // accumulators live in prepared layout

buf := ctx.NewBuffer()
for each incoming slice {
    ctx.Prepare(buf, slice)      // once per input
    ctx.MulAdd(acc, buf, coeff)  // fold; input can then be discarded
}

out := make([]byte, sliceSize)
ctx.Finish(acc, out)             // untransform once, at the end
```

A `Context` is not safe for concurrent `Mul*` calls — use one per goroutine.
`Prepare`/`Finish` are stateless and safe to share. `MulAddMulti` batches
several sources into one accumulator per call and supports stride-aligned
sub-ranges for data parallelism.

## Performance

Benchmarks on Apple M-series (arm64, method "CLMul (SHA3)"), reconstructing
missing shards with `rsec16.Coder.ReconstructData`:

| Benchmark | cgo (SIMD) | pure Go | speedup |
|---|---|---|---|
| Reconstruct 100 shards ×64 KiB, 10 missing | 7042 MB/s | 1570 MB/s | 4.5× |
| Reconstruct 1000 shards ×64 KiB, 50 missing | 1945 MB/s | 327 MB/s | 6.0× |
| Reconstruct 100 shards ×1 MiB, 10 missing | 8086 MB/s | 1728 MB/s | 4.7× |
| GenerateParity 100 shards ×64 KiB, 10 parity | 6549 MB/s | 1576 MB/s | 4.2× |

(On amd64 the gap vs the pure-Go backend is larger for repair: the pure-Go
path there has SSSE3 assembly for encode-style loops but the SIMD backend
adds AVX2/AVX-512/GFNI.)

Reproduce with:

```bash
go test ./rsec16/ -bench . -benchtime 2s
CGO_ENABLED=0 go test ./rsec16/ -bench . -benchtime 2s
```

## Backends

| Build | Backend | Methods |
|---|---|---|
| `CGO_ENABLED=1` (supported platforms) | ParPar kernels | runtime-selected: Shuffle (SSSE3/AVX2/AVX-512/VBMI), Affine (GFNI), XOR-JIT, CLMul (NEON/SHA3/SVE2), … |
| `CGO_ENABLED=0` or other platforms | pure Go (`gf2p16`) | SSSE3 assembly on amd64, scalar elsewhere |

Both backends produce bit-identical output; the test suite runs every test
against whichever backend is built, and CI runs both.

## Rebuilding the static libraries

```bash
make -C gf16 darwin        # macOS universal (arm64 + x86_64)
make -C gf16 linux/amd64   # on a linux/amd64 host
make -C gf16 linux/arm64   # on a linux/arm64 host
make -C gf16 windows/amd64 # in an MSYS2/MinGW64 shell
```

`.github/workflows/build-libs.yml` builds all four in CI and commits the
resulting `gf16/libgf16_*.a` back to the branch. It runs automatically on any
push that touches `gf16/vendor/**`, `gf16/csrc/**`, `gf16/bridge.h` or
`gf16/Makefile`, and can also be started manually from the Actions tab
(`workflow_dispatch`).

The Windows lane pins MinGW GCC to a fixed version (`15.2.0-14`). MSYS2 is a
rolling release and GCC 16 dropped the emulated-TLS `std::call_once` symbols
that older libstdc++ emitted, so an unpinned toolchain produces a library that
fails to link in consumers. Downstream projects linking the prebuilt Windows
archive must pin the same version.

## Licensing

- gopar-turbo: MIT ([LICENSE](LICENSE)).
- Forked gopar packages (`gf2`, `gf2p16`, `rsec16`, `par2`, `memfs`): MIT,
  © Frederick Akalin ([LICENSE.gopar](LICENSE.gopar)).
- Vendored ParPar gf16 kernels (`gf16/vendor`): CC0 / public domain
  ([gf16/vendor/LICENSE](gf16/vendor/LICENSE)).
- **No code from par2cmdline-turbo (GPLv2) is included.** Its kernels are the
  same CC0 ParPar sources vendored here directly.
