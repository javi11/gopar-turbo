# Performance parity with par2cmdline-turbo

Date: 2026-08-24
Status: approved, not yet implemented
Builds on: [2026-08-24-streaming-memory-design.md](2026-08-24-streaming-memory-design.md)

## Problem

After the bounded-memory work, gopar-turbo is correct and memory-sane but
still loses to par2cmdline-turbo on wall-clock. Full 4.36 GiB set, Apple M4
(10 cores), warm cache, cgo backend:

| Scenario | gopar-turbo | par2cmdline-turbo | gap |
|---|---:|---:|---:|
| verify-intact | 17.40s | 4.21s | 4.1× |
| verify-damaged | 16.74s | 9.63s | 1.7× |
| repair-missing (5 files) | 93.41s | 9.06s | 10.3× |
| repair-corrupt (200 slices) | 99.94s | 18.89s | 5.3× |

Both tools run the same vendored ParPar GF(2^16) kernels, so no gap is
kernel-level. The measured causes:

1. **The streaming fold is sequential.** `repair-missing` performs
   1965 inputs × 2,380,956 B × 210 outputs ≈ 915 GB of GF MulAdd. At the
   kernels' ~10 GB/s/core that is ~92s on one core — which is what we
   measure (93.4s). Ten cores put the floor at ~9.2s, i.e. par2cmdline-turbo
   is already *at* the shared-kernel compute floor.
2. **The scan is single-threaded.** Verify hashes 4.36 GB of MD5 + CRC32 +
   16k-hash on one core at ~256 MB/s. par2cmdline-turbo spreads it across
   cores at ~1 GB/s aggregate.
3. **Recovery volumes are read whole.** ~700 MB of volume files inflate
   verify RSS to ~890 MB and add serial I/O/parse time.

## Goal

Match or beat par2cmdline-turbo's wall-clock on all four benchmark scenarios
with the cgo backend, while:

- output stays byte-identical on both backends (equivalence oracles unchanged),
- peak RSS does not regress from the current numbers (and improves where the
  volume work lands),
- the gopar-compatible public API shape is unchanged (additive only, and
  nothing new is expected to be needed).

The compute floor belongs to both tools equally — same kernels, same 915 GB
of MulAdd. Our own in-repo benchmark already demonstrates the required
throughput: `BenchmarkReconstruct_1000x50_64K` moves 1945 MB/s of input
across 50 outputs ≈ 97 GB/s aggregate through the existing parallel matrix
path, which prices the fold at ~9.4s. Reaching par2cmdline-turbo's 9.06s
total therefore requires two things, both design work rather than physics:
the fold must hit the throughput our own benchmark proves, and the scan and
parity load must overlap with fold setup instead of running as sequential
phases. `verify-*` and `repair-corrupt` are winnable outright.

## Non-goals

- Kernel changes (`gf16/vendor` stays untouched).
- Creation/encode performance.
- Matching par2cmdline-turbo's ~10 MB verify RSS exactly; ~90 MB
  (scan window + presence bookkeeping) is the target after the volume work.

## Design

Three workstreams, independent and separately mergeable.

### 1. Parallel, batched, pipelined fold (`rsec16.FoldInputs`)

Adopt the concurrency pattern already proven in `applyMatrixGF16`
(rsec16/apply_gf16.go): **one `gf16.Context` per worker** (contexts share
`mutScratch`, so they cannot be shared across concurrent `Mul*` calls; a
context is small — the buffers are what is big), **shared read-only prepared
inputs**, and a queue of **(output row × stride-aligned byte range) units**
each executing one `MulAddMulti`.

The fold processes inputs in **batches of B** (default 16, capped by the
memory budget):

1. A **reader goroutine** fetches inputs `next(j, buf)` and `Prepare`s them
   into one of two batch buffers (double buffering: batch N+1 is read and
   prepared while batch N is folded). `Prepare` is stateless and safe there.
2. When a batch is full, workers drain a unit queue: unit (row `i`,
   range `[off, off+len)`) executes
   `wctx.MulAddMulti(accs[i], batch, off, len, coeffs[i][batchSlice])` —
   one pass over each accumulator range per batch instead of per input,
   cutting accumulator cache traffic by ~B×.
3. Barrier between batches (the accumulator set is shared); `Finish` all
   accumulators once at the end, in parallel, exactly as `applyMatrixGF16`
   does.

Memory: `2 × B × bufSize` for batch buffers (~76 MB at B=16 on this set) plus
the existing per-missing-shard accumulators. `chunkSizeFor` (the
`MemoryBudget` chunking) now accounts for batch buffers too; when the budget
is tight it shrinks B before it shrinks the chunk, since small chunks cost
extra passes over all inputs.

**Throughput gate before integration.** The first deliverable is a
`FoldInputs` benchmark shaped like the real workload (≥1000 inputs,
2,380,956-byte slices, ≥200 outputs) with in-memory `next`. The parallel
fold must reach the aggregate GB/s of `BenchmarkReconstruct_1000x50_64K`'s
matrix path (~97 GB/s on this host) before any par2 integration lands; unit
range size (target: fits L2 alongside a batch's source ranges) and batch
size B are tuned against this benchmark, not guessed.

**Phase overlap in repair.** `LoadParityData` (volume I/O + parse) runs
concurrently with `LoadFileData` (the scan) — they touch disjoint files and
disjoint decoder fields, and both must complete before reconstruction plans.
This hides most of the ~4s scan behind parity I/O and puts total repair time
at fold-time + ε rather than scan-time + fold-time. Verify keeps the same
overlap (`LoadParityPresence` alongside the scan) for its own win.

The **pure-Go path** gets the same row-split parallelism (workers own
disjoint output rows; `MulAndAddByteSliceLE` into rows they own; inputs
shared read-only) so `CGO_ENABLED=0` scales too. Batch pipelining applies
there unchanged.

`FoldInputs`' signature is unchanged; `numGoroutines` stops being decorative.
The `next` callback contract gains one word: it may be called **ahead** of
the fold (prefetch), still strictly in order `j = 0..numInputs-1`, still one
at a time (the reader is a single goroutine), so `readerCache` in par2 needs
no locking.

### 2. Parallel scan (`Decoder.LoadFileData`)

A worker pool of `numGoroutines` scans files concurrently. Per file, the
work is exactly today's `fillFileIntegrityInfos` — windowed read, MD5,
16k-hash, rolling CRC32 — which is fully independent per file and is ~99% of
the cost.

Shared state and its protection:

- `checksumToLocation` is built before the scan and is **read-only** during
  it — no locking.
- Recording a match writes to `fileIntegrityInfos[k]` where `k` is the file
  that *owns* the matched shard — not necessarily the file being scanned
  (duplicate slices; misaligned finds). One mutex guards the
  match-recording block in `scanBuffer`. Matches are rare relative to bytes
  hashed, so contention is negligible.
- `OnDataFileLoad` delegate callbacks are emitted from a single collector in
  file order (workers report results over a channel), preserving today's
  observable ordering. Per-window delegate calls that fire mid-scan
  (`OnDetectCorruptDataChunk`, hash-mismatch) are serialized with the same
  mutex.

First error wins and cancels remaining work (errgroup-style; implemented
with the stdlib since the repo has no golang.org/x dependency).

Determinism note: when the *same* shard content appears at multiple
locations, today's sequential scan records the first location in file order;
the parallel scan records whichever scanner finds it first. `foundAt` is
only used to re-read bytes that are identical by construction (CRC32+MD5
match), so output bytes are unaffected. `locations` sets are unordered
already.

### 3. Streaming parity load (`loadParityData`)

Parse each volume file through a window instead of `ReadFile`:

- Walk packets by header (`PAR2\0PKT`, 8-byte length): read the 64-byte
  header, then either **skip** (presence mode: seek past the body after
  noting the exponent; non-recovery packets always skipped once the index
  has been validated) or **copy out** the recovery block body (repair mode)
  directly into its own buffer.
- Packet MD5 validation is preserved: hash header+body while copying; in
  presence mode, hash while skipping through the window (the bytes are read
  either way — "skip" saves retention, not I/O).
- Volumes are processed by a small worker pool (they are independent files);
  the exponent→shard table is assembled under a mutex.

Verify drops from ~890 MB to ~90 MB on the full set. Repair still retains
recovery-block bytes (they are fold inputs), but no longer the volume file
buffers around them.

An alternative — deferring recovery-block reads until the fold requests
them — was considered and rejected for now: it complicates the fold's input
contract for ~500 MB that the budget already accounts for. Revisit only if a
future target machine can't hold the used recovery blocks.

## What is deliberately not touched

- `Repair`'s file assembly/write path (already ~1s).
- `verifyParity` (only runs under `DoubleCheck`, not benchmarked) — it
  inherits the faster fold automatically since it calls `FoldInputs`.
- The misaligned-search semantics (`scanPolicy`) — the parallel scan wraps
  the existing per-file scanner unchanged.

## Testing

Test-first per workstream; the equivalence oracles from the memory work
remain the ground truth.

- **Fold:** existing `TestFoldInputs*` equivalence tests must pass unchanged
  (they compare against `applyMatrix`); add cases exercising `numGoroutines`
  1 vs N and batch boundaries (numInputs not divisible by B; B > numInputs;
  chunk < slice with batching). A prefetch-order test asserts `next` is
  called sequentially exactly once per input. `-race` on both backends.
- **Scan:** existing scan tests pass with the pool; add a determinism test
  (same `ShardCounts` and same repaired bytes with goroutines=1 vs 8 on a
  set with duplicate and misaligned shards); delegate-order test for
  `OnDataFileLoad`.
- **Parity:** presence and retain modes against `buildPAR2Data` fixtures and
  the realtool fixtures; a corrupted-packet-MD5 volume is rejected
  identically to today.
- **End to end:** full `-race` suite both backends, then `bench/run.py` on
  the 1 GiB and 4.36 GiB sets; all repairs must stay `md5=ok`.

## Expected outcome (full set)

| Scenario | now | target | par2cmdline-turbo |
|---|---:|---:|---:|
| verify-intact | 17.4s | ≤ 4.2s | 4.2s |
| verify-damaged | 16.7s | ≤ 6s | 9.6s |
| repair-missing | 93.4s | ≤ 9.5s (fold ~9.4s at proven 97 GB/s, scan overlapped) | 9.1s |
| repair-corrupt | 99.9s | ≤ 14s | 18.9s |

Verify RSS ~890 MB → ~90 MB; repair RSS unchanged or slightly higher by the
batch buffers (~76 MB), still far under budget.
