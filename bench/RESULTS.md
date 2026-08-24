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
| verify-intact | **gopar-turbo (cgo)** | **2.96s** | 205 MB | — |
| | gopar-turbo (pure Go) | 2.86s | 264 MB | — |
| | par2cmdline-turbo | 3.83s | 8 MB | — |
| | par2cmdline (stock) | 12.50s | 9 MB | — |
| verify-damaged (200 slices) | **gopar-turbo (cgo)** | **2.84s** | 369 MB | — |
| | gopar-turbo (pure Go) | 2.88s | 293 MB | — |
| | par2cmdline-turbo | 9.34s | 12 MB | — |
| | par2cmdline (stock) | 14.95s | 13 MB | — |
| repair-missing (5 files) | **gopar-turbo (cgo)** | **9.02s** | 2133 MB | yes |
| | gopar-turbo (pure Go) | 73.45s | 1616 MB | yes |
| | par2cmdline-turbo | 9.00s | 562 MB | yes |
| | par2cmdline (stock) | 44.54s | 488 MB | yes |
| repair-corrupt (200 slices) | **gopar-turbo (cgo)** | **16.66s** | 2158 MB | yes |
| | gopar-turbo (pure Go) | 76.23s | 1531 MB | yes |
| | par2cmdline-turbo | 18.51s | 540 MB | yes |
| | par2cmdline (stock) | 58.12s | 468 MB | yes |

gopar-turbo (cgo) is at or ahead of par2cmdline-turbo on every scenario:
verify-intact 1.3× faster, verify-damaged 3.3× faster, repair-missing a dead
heat (0.2% apart, inside run noise — both tools sit at the shared kernels'
compute floor of ~915 GB of GF MulAdd), repair-corrupt 1.11× faster.

## Scaled set, 1 GiB

Same slice size and redundancy, first 10 parts. Median of 3 reps.

| Scenario | Tool | Time | Peak RSS | Correct |
|---|---|---:|---:|:---:|
| verify-intact | **gopar-turbo (cgo)** | **0.67s** | 221 MB | — |
| | par2cmdline-turbo | 0.81s | 8 MB | — |
| verify-damaged (50 slices) | **gopar-turbo (cgo)** | **0.66s** | 227 MB | — |
| | par2cmdline-turbo | 2.04s | 12 MB | — |
| repair-missing (1 file) | **gopar-turbo (cgo)** | **1.00s** | 683 MB | yes |
| | gopar-turbo (pure Go) | 3.67s | 592 MB | yes |
| | par2cmdline-turbo | 1.26s | 178 MB | yes |
| repair-corrupt (50 slices) | **gopar-turbo (cgo)** | **2.41s** | 733 MB | yes |
| | gopar-turbo (pure Go) | 5.59s | 570 MB | yes |
| | par2cmdline-turbo | 3.20s | 201 MB | yes |

Every repair in both sweeps produced byte-identical output to the pristine set
(80 runs, zero mismatches).

## How the gap closed

This benchmark originally found gopar-turbo far behind — verify 4.1× slower,
repair-missing 10.3× slower — despite both tools running the same kernels.
Two rounds of work on this branch closed and then reversed it. Full-set
history for the cgo build:

| Scenario | original | after memory work | final | par2cmdline-turbo |
|---|---:|---:|---:|---:|
| verify-intact | — | 17.40s / 889 MB | 2.96s / 205 MB | 3.83s |
| verify-damaged | — | 16.74s / 954 MB | 2.84s / 369 MB | 9.34s |
| repair-missing | (would swap: ~13.7 GB) | 93.41s / 2064 MB | 9.02s / 2133 MB | 9.00s |
| repair-corrupt | — | 99.94s / 1980 MB | 16.66s / 2158 MB | 18.51s |

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

## Where each tool still wins

- **par2cmdline-turbo on memory**: ~10 MB verify / ~550 MB repair vs our
  ~205 MB / ~2.1 GB. gopar-turbo's repair holds one accumulator per missing
  shard (bounded by `MemoryBudget`, here 210 × 2.4 MB plus batch buffers and
  GC headroom); par2cmdline-turbo's chunked design is tighter. On this
  hardware the difference doesn't affect wall-clock.
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
