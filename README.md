# gopar-turbo

SIMD-accelerated **PAR2 verify and repair** for Go, with a
[gopar](https://github.com/akalin/gopar)-compatible API. The GF(2^16)
Reed-Solomon hot loop runs on Go-assembly ports of
[ParPar](https://github.com/animetosho/ParPar)'s SIMD algorithms (NEON/SHA3 on
arm64, SSSE3/AVX2 on amd64) selected at runtime by CPU detection, with a
portable pure-Go fallback. It is plain Go: no cgo, no C toolchain, no
prebuilt libraries — it cross-compiles like any Go module and works with
`CGO_ENABLED=0`.

It is the repair-side companion to [par2go](https://github.com/javi11/par2go):
par2go creates PAR2 sets, gopar-turbo verifies and repairs them. (An encode
path exists because the fork inherits it — and it accelerates for free — but
creation is par2go's job and is not a supported surface here.)

## Install

```bash
go get github.com/javi11/gopar-turbo
```

Requires Go 1.25+ (see `go.mod`). Nothing else is needed: the assembly kernels
are built by the Go toolchain on every platform, and platforms without a
kernel use the pure-Go backend with identical results.

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

### Misaligned data

When a slice matches no known shard, the scanner can fall back to a byte-wise
rolling-CRC search that finds shards sitting *off* a slice boundary — the case
where data has been shifted by an insertion or deletion. That search is
expensive: every miss can slide up to a full slice before it resynchronises,
which on a set with 2 MiB slices costs roughly 25× the scan time of an intact
set. It is therefore **off by default**, matching par2cmdline's `-N` flag:

```go
par2.Verify("recovery.par2", par2.VerifyOptions{
    FindMisalignedData:    true, // off by default
    MisalignedSearchLimit: 64,   // 0 = unbounded (the historical behaviour)
})
```

Leave it off for ordinary corruption and missing files, where damage is
slice-aligned and parity does the work. Turn it on when a file may have been
shifted rather than corrupted in place.

### Memory

Verify streams: it walks each file through a small sliding window and records
only which shards are present, so its memory does not grow with the set. On a
4.36 GiB release the scan itself costs the same as on a 1 GiB one.

Repair reconstructs by folding one input shard at a time into an accumulator
per *missing* shard, re-reading survivors and recovery blocks from disk rather
than holding them, and streaming each rebuilt chunk range straight into the
output file instead of assembling whole files in memory. Repaired files are
written to temporary siblings and renamed on success, so an aborted repair
leaves the originals untouched. On APFS and Linux reflink filesystems the
temporary is a copy-on-write clone of the damaged file, so only the
reconstructed slices are actually written.

Bulk reconstruction buffers default to a budget of at most 512 MiB,
including input staging and both prepared and output accumulators.
`MemoryBudget` can lower that budget or raise it to reduce scattered I/O
on large repairs. Slices are split into byte ranges when needed:

```go
par2.Repair("recovery.par2", par2.RepairOptions{
    MemoryBudget: 64 << 20, // 0 = up to 512 MiB; larger values are also honored
})
```

Smaller budgets increase the number of passes and positional I/O operations.
Matrices, scan windows, metadata and native contexts are additional memory;
this option is not a process RSS cap, and tiny budgets still need minimum-sized,
aligned buffers. The optional `DoubleCheck` reads each data byte once across
its chunk passes and computes only the recovery blocks actually present.

`RepairOptions.NumGoroutines` lets callers tune repair concurrency to reduce CPU
consumption. For example, a workload-tested limit can be set explicitly:

```go
par2.Repair("recovery.par2", par2.RepairOptions{
    NumGoroutines: 8,
})
```

This is a worker setting, not a hard CPU-percentage limit. On the tested 10-core
Apple M4, a worker sweep against commit `b38267b` found that eight workers used 8–16% fewer CPU-seconds than ten, but one large case
took 6% longer. The default remains unchanged. See [CPU measurements](bench/CPU_RESULTS.md)
and the [worker comparison commands](bench/README.md#repair-cpu-consumption) to
choose a setting for your workload; CPU savings without slowdown are not universal.

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

### Go assembly backend vs the former cgo backend

Re-measured on 2026-11-11 against the last commit with the C backend (9c0f5ca),
on an Apple M4 (10 cores, Go 1.27, kernel "CLMul (SHA3)" in both builds). The
set is 10 random 100 MiB files (1000 MiB), 2,380,956-byte slices, 68 recovery
blocks (15%), created with stock par2cmdline; warm cache, median of 10
interleaved reps, every repair MD5-checked against pristine:

| Scenario | cgo (C kernel) | Go asm | cgo peak RSS | Go asm peak RSS | Correct (Go asm) |
|---|---:|---:|---:|---:|:---:|
| verify-intact | 0.20s | 0.20s | 58 MB | 58 MB | — |
| verify-damaged (50 slices) | 0.18s | 0.18s | 58 MB | 58 MB | — |
| repair-missing (1 file) | 0.58s | 0.58s | 414 MB | 406 MB | 10/10 |
| repair-corrupt (65 slices) | 0.70s | 0.67s | 508 MB | 498 MB | 10/10 |

The honest reading is parity, slightly ahead on repair-corrupt: repairs finish
in well under a second here, and the machine was not idle (load average ~3-4).
These are arm64 (Apple M4) numbers only. amd64 performance of the Go assembly
kernels has not been measured on real x86 hardware, the GFNI kernels are behind
the `gf16unverified` build tag and have never been executed, and ParPar's
AVX-512 kernels are not ported, so on GFNI/AVX-512 CPUs the default build is
expected to be slower than the former C backend until those are validated.
Details, `rsec16` package benchmarks and caveats:
[bench/RESULTS.md](bench/RESULTS.md#2026-11-11-go-assembly-backend-vs-the-former-cgo-parpar-c-backend).

> The tables below this point are historical: they were measured with the
> previous cgo/C kernel backend, before the port to Go assembly, and have not
> been re-measured.

End to end on a real 4.36 GiB Usenet release (47 files, 2,380,956-byte
slices, 295 recovery blocks, 15% redundancy) on an Apple M4 with 10 cores,
warm cache, against [par2cmdline-turbo](https://github.com/animetosho/par2cmdline-turbo)
1.5.0 — which vendors the *same* ParPar kernels, so the comparison is of
everything around them:

| Scenario | gopar-turbo (cgo) | par2cmdline-turbo | par2cmdline 1.3.0 |
|---|---|---|---|
| verify, intact | **2.99s** / 199 MB | 3.89s / 8 MB | 12.44s / 9 MB |
| verify, 200 damaged slices | **2.87s** / 305 MB | 9.36s / 12 MB | 15.02s / 13 MB |
| repair, 5 missing files | **8.88s** / 858 MB | 9.29s / 560 MB | 44.8s / 488 MB |
| repair, 200 corrupt slices | **16.58s** / 1015 MB | 18.37s / 540 MB | 58.4s / 468 MB |

Faster on every scenario, with byte-identical repairs (80 benchmark runs,
zero mismatches). par2cmdline-turbo still uses less memory: repair is
1.5–1.9× its footprint, and verify is far above it — the parallel scan's
per-worker windows are what buy the verify speed. Methodology, a 1 GiB
variant, the pure-Go numbers, and the full history are in
[bench/RESULTS.md](bench/RESULTS.md); reproduce with `bench/run.py`
(see [bench/README.md](bench/README.md)).

Kernel-level benchmarks on Apple M-series (arm64, method "CLMul (SHA3)"),
reconstructing missing shards with `rsec16.Coder.ReconstructData`:

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
GOPAR_GF16_KERNEL=purego go test ./rsec16/ -bench . -benchtime 2s
```

## Backends

The `gf16` package picks the fastest kernel the CPU supports at startup:

| Architecture | Kernels (best last) | Notes |
|---|---|---|
| arm64 | `CLMul (NEON)`, `CLMul (SHA3)` | PMULL carry-less multiply; SHA3 adds `EOR3` where the CPU has it |
| amd64 | `Shuffle (SSSE3)`, `Shuffle (AVX2)` | PSHUFB nibble tables, built per coefficient from constant tables inside the assembly |
| amd64, `-tags gf16unverified` | adds `Affine2x (GFNI+AVX2)`, `Affine2x (GFNI+AVX512)` | see below |
| anything else | pure Go (`gf2p16`) | also used when forced, see below |

All kernels, and the pure-Go path, produce bit-identical output; the test
suite compares every kernel the host can run against the pure-Go reference,
and CI runs the suite through each of them.

**`gf16unverified` build tag.** The GFNI kernels assemble and vet, but their
instructions could not be executed where they were written, so their
differential tests have never run on real hardware. They are compiled only
with `-tags gf16unverified` and are never part of a default build. On a
GFNI machine, `go test -tags gf16unverified ./gf16` runs the same tests
against them (the tests pick up any kernel the CPU can run); the tag is
meant to be dropped once that has been verified.

**`GOPAR_GF16_KERNEL`.** Set this environment variable to the exact kernel
name (as `Context.MethodName()` reports it, e.g. `CLMul (NEON)`) or to
`purego` to override the default choice, for example to run the whole stack
on a non-default kernel or to compare against the portable path:

```bash
GOPAR_GF16_KERNEL="Shuffle (SSSE3)" go test ./rsec16 ./par2
GOPAR_GF16_KERNEL=purego go test ./rsec16/ -bench .
```

It is read once at startup. An unknown name, or a kernel the CPU cannot run,
is ignored and the default is kept.

The kernel assembly is generated: `go generate ./gf16/...` runs
`gf16/asmgen` to rewrite `clmul_arm64.s`, `shuffle_amd64.s` and
`affine_amd64.s` (run it with `GOARCH=amd64` for the amd64 files). The
committed files must equal the generator output.

`go run asmgen/x86.go -surrogate=<file>` (from `gf16/`) writes a development
only variant of the affine kernels with `GF2P8AFFINEQB` replaced by XOR, to a
file other than `affine_amd64.s`; its products are wrong, it carries a loud
marker, and a test fails if it ever lands in `affine_amd64.s`.

## Licensing

- gopar-turbo: MIT ([LICENSE](LICENSE)).
- Forked gopar packages (`gf2`, `gf2p16`, `rsec16`, `par2`, `memfs`): MIT,
  © Frederick Akalin ([LICENSE.gopar](LICENSE.gopar)).
- The `gf16` assembly kernels and their generators are ports of ParPar's
  algorithms (CC0 / public domain); attribution and the dedication text are
  in [gf16/LICENSE.parpar](gf16/LICENSE.parpar).
- **No code from par2cmdline-turbo (GPLv2) is included.**

## Deep repair planning

Disk repairs with at least 1,024 missing blocks, at least 8,388,608 data×missing
pairs, consecutive selected recovery exponents and a buffer budget of at least
64 MiB use an additive GF(2¹⁶) transform. This avoids allocating and inverting a
dense reconstruction matrix. Consecutive smaller repairs use closed-form
coefficients where beneficial; irregular exponent sets retain the general
matrix solver. The ordinary `rsec16.Coder` API retains matrix reconstruction.

The transform workspace is reused across byte ranges and released after the
repair. The default 512 MiB budget covers bulk arithmetic buffers, including
native allocations; plans, scan buffers and other metadata add to process RSS.
See [deep repair measurements](bench/RESULTS.md) and the
[reproducible comparison](bench/README.md). Measurements cover both 16 KiB and
128 KiB block fixtures and report remaining gaps against parfast. They do not
establish a bound for cold disks or reproduce the reported hour-long production
repair.
