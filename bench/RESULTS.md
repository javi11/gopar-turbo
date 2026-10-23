# gopar-turbo vs par2cmdline-turbo

Measured on a real Usenet release: 47 files, 4,678,127,270 bytes (4.36 GiB),
slice size 2,380,956, 295 recovery blocks (15% redundancy).

- Host: Apple M4, 10 cores, 16 GiB RAM, macOS (darwin 25.5.0)
- gopar-turbo backend: ParPar kernels via cgo, method "CLMul (SHA3)"
- par2cmdline-turbo 1.5.0, built from source. It vendors the **same ParPar
  kernels** gopar-turbo does, so differences are architectural, not kernel-level
- par2cmdline 1.3.0 (Homebrew) as a no-SIMD floor
- Warm page cache; each run starts from a fresh copy-on-write clone with
  seeded, byte-identical damage; repaired output is MD5-checked against a
  verified-clean baseline

Reproduce with `bench/run.py` — see [README.md](README.md).

## Full set, 4.36 GiB

| Scenario | Tool | Time | Peak RSS | Correct |
|---|---|---:|---:|:---:|
| verify-intact | **gopar-turbo (cgo)** | **2.99s** | 199 MB | — |
| | gopar-turbo (pure Go) | 2.93s | 262 MB | — |
| | par2cmdline-turbo | 3.89s | 8 MB | — |
| | par2cmdline (stock) | 12.44s | 9 MB | — |
| verify-damaged (200 slices) | **gopar-turbo (cgo)** | **2.87s** | 305 MB | — |
| | gopar-turbo (pure Go) | 2.90s | 285 MB | — |
| | par2cmdline-turbo | 9.36s | 12 MB | — |
| | par2cmdline (stock) | 15.02s | 13 MB | — |
| repair-missing (5 files) | **gopar-turbo (cgo)** | **8.88s** | 858 MB | yes |
| | gopar-turbo (pure Go) | 73.9s | 795 MB | yes |
| | par2cmdline-turbo | 9.29s | 560 MB | yes |
| | par2cmdline (stock) | 44.8s | 488 MB | yes |
| repair-corrupt (200 slices) | **gopar-turbo (cgo)** | **16.58s** | 1015 MB | yes |
| | gopar-turbo (pure Go) | 76.6s | 892 MB | yes |
| | par2cmdline-turbo | 18.37s | 540 MB | yes |
| | par2cmdline (stock) | 58.4s | 468 MB | yes |

gopar-turbo (cgo) is faster than par2cmdline-turbo on every scenario:
verify-intact 1.3×, verify-damaged 3.3×, repair-missing 1.05×,
repair-corrupt 1.11×. Repair memory is 1.5-1.9× theirs, down from 4.2×
before the memory work; verify memory remains far above theirs (see below).

## Rival survey: nzbfast parfast 1.6.0 (2026-09-21)

Same host (Apple M4, 10 cores). A fresh 1000 MiB set of 10 random
100 MiB files, slice size 2,380,956, 450 data blocks, 68 recovery blocks
(15%), created with stock par2cmdline 1.3.0 so no contestant's creator
shaped the layout. Median of 3 reps (6 for the parfast default rows);
gopar-turbo commit 72db3e2. [parfast](https://github.com/nzbfast/nzbfast)
is nzbfast's pure-Rust PAR2 tool with par2cmdline's command dialect; it
was run from the macOS universal release binary.

| Scenario | Tool | Time | Peak RSS | Correct |
|---|---|---:|---:|:---:|
| verify-intact | gopar-turbo (cgo) | 1.04s | 225 MB | — |
| | gopar-turbo (pure Go) | 1.17s | 266 MB | — |
| | par2cmdline-turbo | 1.12s | 10 MB | — |
| | **parfast** | **0.22s** | 10 MB | — |
| | parfast --slow | 0.18s | 10 MB | — |
| verify-damaged (50 slices) | gopar-turbo (cgo) | 0.80s | 236 MB | — |
| | gopar-turbo (pure Go) | 0.85s | 266 MB | — |
| | par2cmdline-turbo | 2.67s | 10 MB | — |
| | **parfast** | **0.24s** | 41 MB | — |
| | parfast --slow | 0.12s | 31 MB | — |
| repair-missing (1 file) | gopar-turbo (cgo) | 1.27s | 369 MB | yes |
| | gopar-turbo (pure Go) | 7.79s | 195 MB | yes |
| | par2cmdline-turbo | 1.40s | 184 MB | yes |
| | **parfast** | **0.63s** | 482 MB | yes |
| repair-corrupt (50 slices) | gopar-turbo (cgo) | 3.49s | 400 MB | yes |
| | gopar-turbo (pure Go) | 12.81s | 215 MB | yes |
| | par2cmdline-turbo | 3.67s | 205 MB | yes |
| | **parfast** | **1.00s** | 512 MB | yes |
| | parfast --slow | 0.83s | 1.15 GB | yes |

parfast is faster than gopar-turbo (cgo) on every scenario: verify-intact
4.7×, verify-damaged 3.3×, repair-missing 2.0×, repair-corrupt 3.5×. Every
parfast repair was byte-identical to the pristine set. Its `--slow` flag
takes the verdict from the whole-file MD5 (as par2cmdline and gopar-turbo
do) rather than its default per-block checksums; it is not slower here.

### After the scan and repair pipeline work (same day)

Same set, same host, same protocol, gopar-turbo rebuilt from this branch.
Median of 3.

| Scenario | gopar-turbo (cgo) | parfast | ratio |
|---|---:|---:|---:|
| verify-intact | 0.24s / 123 MB | 0.20s / 10 MB | 1.24× |
| verify-damaged (50 slices) | 0.23s / 123 MB | 0.23s / 41 MB | 0.99× |
| repair-missing (1 file) | 0.65s / 451 MB | 0.60s / 492 MB | 1.09× |
| repair-corrupt (50 slices) | **0.74s** / 440 MB | 0.82s / 512 MB | **0.90×** |

Every repair byte-identical to the pristine set. Against the numbers above
that is verify 4.3× and 3.5× faster, repair 2.0× and 4.7× faster, with
verify memory halved. The pure-Go backend now verifies as fast as the cgo
one (0.25s), since verify never touches the kernels.

What changed, none of it in the kernels:

- **One hashing pass per byte.** The scan no longer computes a whole-file
  MD5 alongside the per-slice MD5s: a file whose slices all match at their
  canonical offsets is byte-for-byte the original. The rolling-CRC table
  for the misaligned search was being rebuilt per scanned window (ten
  2.4 MB hashes each time); it is now built once per slice size, only when
  that search runs.
- **Window-granular parallelism.** Files are walked in slice-aligned
  windows over one shared worker pool instead of one worker per file, so a
  few large files still use every core, and on asymmetric CPUs (this M4
  has 4 performance and 6 efficiency cores) a whole file is never pinned to
  a slow core.
- **Parallel recovery-volume load.** Volumes were read and hash-checked on
  one goroutine while the scan had the rest; at 160 MB of recovery data
  that took longer than the scan itself. Packets now load on the worker
  pool, and verify reads only recovery packet headers and exponents
  (repair still hash-checks every packet before using it).
- **Repair from a clone.** On APFS and Linux reflink filesystems the
  rewrite starts from a copy-on-write clone of the damaged file and writes
  only the reconstructed or relocated slices, instead of copying every
  surviving slice into a fresh temp file. The post-write check covers
  exactly the slices written (`DoubleCheck` still re-verifies whole files).
  Elsewhere the copy path remains, now parallel across files.
- **Concurrent fold input loading and parallel commit.** Reconstruction
  loads and prepares each batch of inputs on several goroutines
  (`rsec16.FoldInputsParallel`); commits and post-write checks run across
  files instead of one at a time.

The remaining verify gap is the recovery-volume header walk plus scan
setup; the scan alone measures 0.20s, parfast's number.

### Where the verify gap comes from

CPU time, not parallelism. On the intact set (`time`, warm cache):

| Tool | user CPU | CPU utilisation | wall |
|---|---:|---:|---:|
| `md5` CLI, 10 files in parallel (floor) | 1.47s | 508% | 0.34s |
| parfast | 1.50s | 770% | 0.21s |
| par2cmdline-turbo | 1.45s | 174% | 0.88s |
| gopar-turbo (cgo) | 4.73s | 711% | 0.71s |

parfast and par2cmdline-turbo both spend exactly the cost of one MD5 pass
over the data; parfast simply spreads it over all cores. gopar-turbo
already has the parallelism but burns **3.2× the CPU** of a single MD5
pass, so the scan is doing redundant hashing work (whole-file MD5 plus
per-slice MD5 plus the rolling CRC window over the same bytes). That is
the lever: cutting the verify scan to one hashing pass per byte would put
verify at parity with parfast without touching the kernels.

Repair is a smaller gap and is dominated by the reconstruction loop, where
parfast's fold and its single-plan scheduling win about 2-3.5× at this set
size; the memory trade goes the other way (parfast peaks 1.3× higher on
repair-missing, and 2.9× higher with `--slow` on repair-corrupt).

Reproduce: `bench/run.py ... --parfast /path/to/parfast` adds both parfast
rows; `--skip-tools stock,misalign` was used here.

## Scaled set, 1 GiB

Same slice size and redundancy, first 10 parts. Median of 3 reps.

| Scenario | Tool | Time | Peak RSS | Correct |
|---|---|---:|---:|:---:|
| verify-intact | **gopar-turbo (cgo)** | **0.68s** | 232 MB | — |
| | par2cmdline-turbo | 0.82s | 8 MB | — |
| verify-damaged (50 slices) | **gopar-turbo (cgo)** | **0.66s** | 243 MB | — |
| | par2cmdline-turbo | 2.04s | 12 MB | — |
| repair-missing (1 file) | **gopar-turbo (cgo)** | **1.05s** | 344 MB | yes |
| | gopar-turbo (pure Go) | 3.70s | 331 MB | yes |
| | par2cmdline-turbo | 1.26s | 178 MB | yes |
| repair-corrupt (50 slices) | **gopar-turbo (cgo)** | **2.67s** | 396 MB | yes |
| | gopar-turbo (pure Go) | 5.62s | 372 MB | yes |
| | par2cmdline-turbo | 3.20s | 201 MB | yes |

Every repair in both sweeps produced byte-identical output to the pristine set
(80 runs, zero mismatches).

## How the gap closed

This benchmark originally found gopar-turbo far behind — verify 4.1× slower,
repair-missing 10.3× slower — despite both tools running the same kernels.
Three rounds of work on this branch closed and then reversed it. Full-set
history for the cgo build:

| Scenario | original | after round 1 | final | par2cmdline-turbo |
|---|---:|---:|---:|---:|
| verify-intact | — | 17.40s / 889 MB | 2.99s / 199 MB | 3.89s / 8 MB |
| verify-damaged | — | 16.74s / 954 MB | 2.87s / 305 MB | 9.36s / 12 MB |
| repair-missing | (would swap: ~13.7 GB) | 93.41s / 2064 MB | 8.88s / 858 MB | 9.29s / 560 MB |
| repair-corrupt | — | 99.94s / 1980 MB | 16.58s / 1015 MB | 18.37s / 540 MB |

**Round 1 — correctness of scale** (memory): the misaligned-data search
became opt-in like par2cmdline's `-N` (a damaged verify had cost 25× an
intact one); verify stopped retaining shard payloads and scans through a
sliding window; repair reconstructs by a streaming fold bounded by
`MemoryBudget` instead of holding the set three times over.

**Round 2 — speed**: the fold became parallel (per-worker gf16 contexts),
batched (`MulAddMulti`, 16 inputs per accumulator pass), pipelined (a reader
prepares the next batch while workers fold), and cache-scheduled (units
dispatched range-major in 128 KiB ranges so a batch's sources stay hot across
all accumulators — 13 → 98 → 159 GB/s aggregate on the repair-shaped
benchmark). The file scan runs on a worker pool; the scan and parity load
overlap; recovery volumes are parsed packet-by-packet instead of read whole.

**Round 3 — repair memory**: recovery blocks are read from their volumes by
recorded offset instead of retained (−675 MB); the default accumulator
footprint is capped at 256 MB (chunked passes at that size measured free);
and reconstruction streams each chunk range straight into the output file
through a positional writer instead of accumulating rebuilt shards and
assembling whole files (−500 MB). Writers go to temp siblings renamed on
commit, so an aborted repair now leaves the originals untouched.

## Where each tool still wins

- **par2cmdline-turbo on memory**, though the gap is much smaller than it
  was. Repair: ~860-1015 MB vs their ~540-560 MB (1.5-1.9×, down from 4.2×).
  What remains is capped accumulators (256 MB), the fold's prepared batch
  banks, one open writer per file being rewritten — `repair-corrupt` rewrites
  46 files and costs ~160 MB more than `repair-missing`'s 5 — and Go's GC
  headroom, which is proportional and not something a library should override
  with `SetMemoryLimit`. Verify is the wider gap: ~200-305 MB vs their ~10 MB,
  which is the parallel scan's per-worker windows and the runtime floor. It
  buys the 1.3-3.3× verify speed win and is small in absolute terms, but it
  will never read "8 MB".
- **gopar-turbo (cgo) on time**: every benchmarked scenario, modestly on
  repair-missing (noise), clearly on damaged verify.
- The pure-Go build (`CGO_ENABLED=0`) trails cgo ~8× on repair (scalar GF on
  arm64) but beats stock par2cmdline on verify and is portable anywhere Go
  runs.

## A note on the source data

The release as downloaded was **damaged**: 20 files, 41 of 1965 blocks lost to
Usenet article loss, independently confirmed by par2cmdline-turbo. It was
repaired before benchmarking. A baseline taken from damaged files scores every
correct repair as a mismatch, so verify the set is clean before trusting a run.

## 2026-10-21: large-repair scaling

Compared baseline `680a5da` against the repair performance changes on an Apple
M4 (darwin/arm64). These are component benchmarks, not a reproduction of the
reported hour-long production repair. Both versions ran the same benchmark
functions sequentially, using `-benchtime=3x -count=1` with cgo enabled:

| Benchmark | Baseline | Updated |
|---|---:|---:|
| Reconstruct 1,000 × 64 KiB, 50 missing | 36.81 ms | 30.64 ms |
| Reconstruction matrix: 10,000 data, 1,024 missing | 4.916 s | 1.327 s |
| Streaming fold: 256 inputs, 28 outputs, 2,380,956-byte slices | 105.24 ms | 110.67 ms |
| Parallel fold: 32 inputs, one output, 4 MiB slices | 9.00 ms | 6.52 ms |
| Allocations for that 4 MiB fold | 210.02 MB/op | 21.01 MB/op |

The ordinary streaming fold is approximately unchanged at this sample size;
no end-to-end speed claim follows from these component numbers. Matrix setup
improved about 3.7×, and large-slice folding allocates about 90% fewer bytes.
A separate one-iteration pure-Go matrix comparison measured 4.926 s before and
2.665 s after; that smaller sample should be treated as indicative.

The deterministic parity-check read counter uses 8,229 data bytes, 1,024-byte
slices, a 128-byte budget and 32 recovery exponents. Before the change it read
2,106,624 data bytes; afterward it reads exactly 8,229 bytes across all chunk
passes. The same check passes with only exponent 31 available, and still
rejects corrupt stored parity. The updated calculation includes only present
recovery rows. Another end-to-end test covers exponent 65535, which previously
overflowed while growing the recovery-location arrays.

Chunk sizing now accounts for bulk input and output folding buffers. The
256 MiB budget remains the default, but explicit larger budgets are honored
so callers can reduce positional I/O on large sets. This budget excludes
matrices, scanner windows, native contexts and metadata; it is not an RSS cap.
Small budgets permit minimum/alignment overhead. Pure-Go budgeting reflects
its smaller buffer set.

Reproduce the component comparison with:

```sh
go test ./rsec16 -run '^$' \
  -bench 'Benchmark(FoldInputs_Realistic|FoldInputsParallelLargeSlice|Reconstruct_1000x50_64K|ReconstructionMatrixHeavyDamage)$' \
  -benchtime=3x -count=1
```

For baseline comparison, add the new benchmark entry points to a checkout of
`680a5da` and keep its production implementation unchanged. Cold HDD/network
storage and production repair concurrency remain unmeasured. Byte-identical
heavy-damage repairs, partial final slices, adaptive-batch partial reads and
errors, sparse exponents, and the optimized matrix solver are regression tested.

## 2026-10-22: deep repair transform versus parfast 1.7.1

Apple M4, darwin/arm64, 10 workers, cgo CLMul(SHA3), warm APFS storage.
The deterministic fixture contains 10,000 × 16 KiB data blocks (156.25 MiB)
and 8,192 recovery blocks. Seed 1337 selects distinct blocks to damage, with
64 bytes overwritten in each. Every run starts from a fresh clone, reads the
set into cache, repairs in a separate process and validates all ten data files
with SHA-256. Times include the complete repair command. Staging, cache warming
and checksum validation are excluded. Defaults are used for both tools; Go's
bulk buffer budget is 256 MiB, and memory budgets are not equalized.

Three sequential runs per tool/depth, alternating tool order, give these
medians. All 24 repairs passed checksum validation:

| Missing blocks | gopar transform | parfast 1.7.1 | gopar peak RSS | parfast peak RSS |
|---|---:|---:|---:|---:|
| 1,024 | 1.882 s | 0.374 s | 378.4 MiB | 379.9 MiB |
| 2,048 | 2.115 s | 0.463 s | 383.9 MiB | 507.5 MiB |
| 4,096 | 2.446 s | 0.690 s | 388.2 MiB | 706.5 MiB |
| 8,192 | 3.264 s | 1.104 s | 396.0 MiB | 878.5 MiB |

Before the transform, the already-improved matrix implementation measured
4.552 s at 1,024 missing and 13.719 s at 2,048 missing on the same seeded
fixture (single preliminary samples, not medians). The old cubic planner was
not run at the deepest two depths. These comparisons indicate substantial
improvement, but **parfast remains approximately 3–5× faster** on this fixture.
The transform's fixed full-field passes are still expensive. The reported
hour-long production case has not been reproduced, and cold disks, network
storage, concurrent repairs and other CPUs remain unmeasured.

The solver uses independently implemented Cantor-basis additive transforms,
closed-form consecutive-exponent coefficients, batched SIMD operations and a
reused native workspace. Irregular exponent sets retain matrix decoding.
A checksum-correct rational survivor product-tree experiment was removed
because it did not consistently improve runtime. Fixed smaller byte chunks
also failed to improve runtime consistently; chunk sizing remains budget-based.

Reproduce using `bench/deep.py` and the fixture commands in
[bench/README.md](README.md). The downloaded parfast macOS universal 1.7.1
archive SHA-256 was
`2301bd89c0d57505f5adbba17261b9ee334c17777265983c6dca5f77c3da81a8`.
The harness writes binary/fixture hashes, exact arguments, per-run logs,
exit/timeout status, checksum results, wall time and peak RSS. Failed runs
are explicitly marked invalid and cause a nonzero harness exit.

A separate pure-Go threshold check (one run per case, same fixture) measured
12.818 → 3.535 s at 1,024 missing and 24.411 → 3.262 s at 2,048 missing
when switching from the new closed-form matrix path to the transform. All
four repairs passed SHA-256. This supports using the transform threshold on
this pure-Go platform too; the smaller sample does not establish portable
speedups. The matrix variant changed only transform selection.

Raw repeated cgo runs and fixture/binary hashes are preserved in
[deep-results-2026-10-22.json](deep-results-2026-10-22.json).


## 2026-10-23: transposed moments, parallel writes and lazy tables

These results supersede the historical full-field transform measurements above.
Apple M4, darwin/arm64, Go 1.27.0, native CLMul(SHA3), 10 workers, warm APFS.
Both fixtures contain 10,000 data blocks and 8,192 recovery blocks: 156.25 MiB
of data with 16 KiB blocks and 1.221 GiB with 128 KiB blocks. Damage, fresh
clones, complete-command timing and SHA-256 validation follow the earlier
method. Binaries were warmed before timing. Each tool uses its defaults:
gopar has a 512 MiB bulk buffer budget; parfast's budget is not equalized.
Budgets exclude metadata and are not RSS caps.

Three sequential repetitions per tool/depth alternate tool order. All **48
repairs passed SHA-256 validation**. Tables show median wall time and median
per-process peak RSS. Individual runs vary; all samples are preserved.

### 16KiB blocks

| Missing blocks | gopar | parfast 1.7.1 | gopar peak RSS | parfast peak RSS |
|---|---:|---:|---:|---:|
| 1,024 | 0.480 s | 0.400 s | 225.8 MiB | 381.3 MiB |
| 2,048 | 0.604 s | 0.486 s | 242.0 MiB | 510.1 MiB |
| 4,096 | 0.813 s | 0.698 s | 292.5 MiB | 702.3 MiB |
| 8,192 | 1.241 s | 1.213 s | 401.2 MiB | 878.0 MiB |

### 128KiB blocks

| Missing blocks | gopar | parfast 1.7.1 | gopar peak RSS | parfast peak RSS |
|---|---:|---:|---:|---:|
| 1,024 | 2.210 s | 2.889 s | 527.6 MiB | 1496.8 MiB |
| 2,048 | 2.785 s | 3.125 s | 529.9 MiB | 1709.0 MiB |
| 4,096 | 3.616 s | 4.224 s | 519.6 MiB | 2102.6 MiB |
| 8,192 | 5.776 s | 6.993 s | 540.1 MiB | 2297.4 MiB |

The degree-limited solver, fused SIMD operations, reused workspaces, batched
parity reads, aligned chunks and concurrent writes across files remove much
of the earlier deep-repair overhead. Lazy generic multiplication tables avoid
initializing 64 MiB that native repair does not need.

The larger fixture now beats parfast at every measured depth. The smaller
fixture remains approximately 2–24% slower, so the goal of matching parfast
across the entire measured ladder is **not yet met**. These measurements do
not reproduce the reported hour-long repair or establish performance on cold
HDDs, network storage, concurrent jobs, other CPUs or irregular exponent sets.
Irregular exponents retain the general matrix fallback.

Validation includes the full native and pure-Go suites, race checks covering
GF arithmetic and repair, repeated strict-cgo/aggressive-GC checks, vet for
both backends, and an amd64 pure-Go test-binary cross-build (not executed).
Independent reviews covered solver arithmetic, native lifetimes, parallel
writes and lazy table publication. The concurrent write failure test checks
that all writes finish before abort and that failed repairs do not commit.

Reproduce with [deep.py](deep.py) and [the fixture instructions](README.md).
Final binary/fixture hashes, arguments and all 48 samples are in
[deep-results-2026-10-23.json](deep-results-2026-10-23.json).
