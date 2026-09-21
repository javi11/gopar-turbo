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
