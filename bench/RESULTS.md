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
| verify-intact | gopar-turbo (cgo) | 17.40s | 889 MB | — |
| | gopar-turbo (pure Go) | 17.16s | 955 MB | — |
| | **par2cmdline-turbo** | **4.21s** | **8 MB** | — |
| | par2cmdline (stock) | 12.47s | 9 MB | — |
| verify-damaged (200 slices) | gopar-turbo (cgo) | 16.74s | 954 MB | — |
| | gopar-turbo (pure Go) | 16.67s | 954 MB | — |
| | **par2cmdline-turbo** | **9.63s** | **13 MB** | — |
| | par2cmdline (stock) | 15.20s | 13 MB | — |
| repair-missing (5 files) | gopar-turbo (cgo) | 93.41s | 2064 MB | yes |
| | gopar-turbo (pure Go) | 337.48s | 1616 MB | yes |
| | **par2cmdline-turbo** | **9.06s** | **563 MB** | yes |
| | par2cmdline (stock) | 46.08s | 488 MB | yes |
| repair-corrupt (200 slices) | gopar-turbo (cgo) | 99.94s | 1980 MB | yes |
| | gopar-turbo (pure Go) | 331.93s | 1570 MB | yes |
| | **par2cmdline-turbo** | **18.89s** | **540 MB** | yes |
| | par2cmdline (stock) | 59.65s | 468 MB | yes |

## Scaled set, 1 GiB

Same slice size and redundancy, first 10 parts. Median of 3 reps.

| Scenario | Tool | Time | Peak RSS | Correct |
|---|---|---:|---:|:---:|
| verify-intact | gopar-turbo (cgo) | 3.56s | 257 MB | — |
| | par2cmdline-turbo | 0.82s | 8 MB | — |
| verify-damaged (50 slices) | gopar-turbo (cgo) | 3.46s | 257 MB | — |
| | par2cmdline-turbo | 2.04s | 13 MB | — |
| | par2cmdline-turbo `-N` | 1.29s | 12 MB | — |
| repair-missing (1 file) | gopar-turbo (cgo) | 6.88s | 610 MB | yes |
| | gopar-turbo (pure Go) | 17.29s | 597 MB | yes |
| | par2cmdline-turbo | 1.26s | 178 MB | yes |
| repair-corrupt (50 slices) | gopar-turbo (cgo) | 9.06s | 663 MB | yes |
| | gopar-turbo (pure Go) | 21.43s | 596 MB | yes |
| | par2cmdline-turbo | 3.30s | 201 MB | yes |

Every repair in both sweeps produced byte-identical output to the pristine set.

## What this measured, and what changed

Benchmarking this release surfaced two problems in gopar-turbo, both fixed in
this branch. The 1 GiB numbers before and after:

| Scenario | Time before | Time after | RSS before | RSS after |
|---|---:|---:|---:|---:|
| verify-intact | 3.09s | 3.56s | 1330 MB | 257 MB |
| verify-damaged | 78.2s | 3.46s | 1252 MB | 257 MB |
| repair-missing | 5.66s | 6.88s | 2148 MB | 610 MB |
| repair-corrupt | 83.1s | 9.06s | 3017 MB | 663 MB |

**Unconditional misaligned-data search.** On a slice miss the scanner advanced
one byte at a time, sliding a whole slice before it could resynchronise —
about 119 million window steps for 50 corrupted slices. par2cmdline gates that
search behind `-N`, off by default; gopar-turbo now does the same.

**Whole-set residency.** Verify held every shard's bytes although it only ever
asked whether a shard was present, and repair held the set three times over:
the shards, the parity, and a prepared copy of every input inside
`applyMatrixGF16`. Verify now streams, and repair folds one input at a time
into an accumulator per missing shard, bounded by `MemoryBudget`.

## Where gopar-turbo still loses

- **Verify is single-threaded.** `NumGoroutines` reaches only the
  Reed-Solomon coder, never the MD5/CRC32 scan, so verify pegs one core while
  par2cmdline-turbo uses all of them. This is the largest remaining gap on
  verify: 17.4s vs 4.2s on the full set.
- **Recovery volumes are still read whole.** Verify's 889 MB is almost entirely
  volume buffers; the scan alone costs 89 MB on the full set and 88 MB on the
  1 GiB one — genuinely independent of set size. Parsing volume packet headers
  by streaming would close most of the remaining gap.
- **Reconstruction is sequential.** `FoldInputs` does not parallelise across
  output rows, because a `gf16.Context` is not safe for concurrent `Mul` calls
  and would need one context per goroutine.

The kernels are not the problem — both tools use the same ParPar code. The gap
is in everything around them.

## A note on the source data

The release as downloaded was **damaged**: 20 files, 41 of 1965 blocks lost to
Usenet article loss, independently confirmed by par2cmdline-turbo. It was
repaired before benchmarking. A baseline taken from damaged files scores every
correct repair as a mismatch, so verify the set is clean before trusting a run.
