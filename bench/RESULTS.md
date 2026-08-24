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
