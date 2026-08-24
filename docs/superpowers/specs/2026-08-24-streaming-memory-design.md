# Bounded-memory verify and repair

Date: 2026-08-24
Status: approved, not yet implemented

## Problem

`gopar-turbo` holds the entire protected set in memory. On a real 4.36 GiB
Usenet release (47 files, slice size 2,380,956, 295 recovery blocks) this makes
repair need roughly 13.7 GB of heap on a 16 GB machine, against 178 MB for
par2cmdline-turbo on the same set.

Measured on a 1 GiB subset of that release (Apple M4, 10 cores):

| Operation | gopar-turbo peak RSS | par2cmdline-turbo |
|---|---:|---:|
| verify-intact | 1.30 GB | 8 MB |
| repair-missing | 2.15 GB | 178 MB |
| repair-corrupt | 3.02 GB | 211 MB |

Three separate copies of the data are live during repair:

1. **Whole-file buffers.** `defaultFileIO.ReadFile` (par2/decoder.go) reads each
   data file with `ioutil.ReadFile`. `fillShardInfos` stores each shard as a
   *subslice* of that buffer, so retaining one shard pins the whole file.
2. **Prepared copies.** `applyMatrixGF16` (rsec16/apply_gf16.go) calls
   `ctx.Prepare` for **every** input up front into `prepared[]`, a second full
   copy of the dataset.
3. **Parity shards and accumulators.**

Verify pays cost 1 for nothing: `shardIntegrityInfo.ok` only tests
`len(info.data) != 0`, so verify needs shard *presence*, never shard *bytes*.

## Goals

- Verify memory independent of set size.
- Repair memory bounded by an explicit budget, defaulting like par2cmdline's
  `-m` (half of physical RAM).
- Bit-identical output to today on both the cgo and pure-Go backends.
- No change to the gopar-compatible public API shape beyond additive options.

## Non-goals

- Changing the PAR2 format or the recovery-set semantics.
- Parallelising the per-file scan (worthwhile, tracked separately).
- Creation/encode path performance.

## Design

Two phases. Neither holds the dataset.

### Phase 1 — streaming scan (verify and repair)

- `fileIO` gains `ReadAt(path string) (io.ReaderAt, int64, func() error, error)`
  alongside `ReadFile`, so the scanner can walk a file without materialising it
  and Phase 2 can re-read an arbitrary shard by offset. `ReadFile` stays for the
  small `.par2` index and volume files, which are read whole today and are not
  the memory problem.
- The scan holds one buffer of `2 * sliceByteCount` rounded up to a whole number
  of read blocks. Two slices is the minimum that lets the misaligned search
  slide a full slice past a boundary without re-reading, and it bounds the
  scanner regardless of slice size.
- The rolling CRC32 and the file MD5 / 16k hash are computed as the window
  advances, so no second pass over the file is needed.
- `shardIntegrityInfo` stops carrying `data []byte`. It records **presence and
  location**: whether the shard was found, and the (file, offset) where it sits.
- Verify terminates after this phase. Memory is O(window), constant in set size.

The misaligned-data search added earlier (`scanPolicy`) is unaffected: it
operates within the window and only changes how far the scan slides on a miss.

### Phase 2 — streaming reconstruction (repair only)

Reconstruction computes `out[i] = sum_j m[i][j] * in[j]`. That is a fold, so
inputs can be consumed one at a time instead of held together:

1. Build the reconstruction matrix. This is coefficient-only arithmetic and
   costs nothing in memory.
2. Allocate accumulators for the **missing shards only**: `k * bufSize`.
3. For each input shard `j` — a present data shard re-read from its recorded
   location, or a recovery block read from a `.par2` volume:
   - `Prepare(buf, shard)` into a reusable buffer,
   - `MulAdd(acc[i], buf, m[i][j])` for every missing output `i`,
   - discard `buf`.
4. `Finish` each accumulator and write the repaired files.

Peak memory becomes `k * sliceSize + O(numGoroutines * sliceSize)`.

### Memory budget

New additive option `MemoryBudget int` (bytes; 0 selects half of physical RAM,
matching par2cmdline). When `k * sliceSize` exceeds the budget, each slice is
split into byte-range chunks and reconstruction makes multiple passes over the
inputs — par2cmdline's chunking strategy.

`Context.MulAddMulti(dst, srcs, offset, length, coeffs)` already accepts
stride-aligned sub-ranges, so chunking needs no new kernel support.

### API changes

- `VerifyOptions` / `RepairOptions` gain `MemoryBudget int`. Additive only.
- `rsec16` gains a streaming fold entry point. `Coder.ReconstructData` stays as
  it is, for compatibility and as the equivalence oracle in tests.
- The pure-Go backend (`applyMatrixParallelData`) gets the same fold path so
  `CGO_ENABLED=0` remains correct and bit-identical.
- `fileIO` gains a streaming read method. `defaultFileIO` and `memfs` both
  implement it.

## Trade-offs

Reconstruction re-reads present shards rather than keeping them, costing one
extra pass over the data in exchange for bounded memory. This is the same trade
par2cmdline makes, and on warm cache it is cheap relative to the GF math.

## Testing

Test-first at each layer.

- **Equivalence.** Streaming reconstruction must produce byte-identical output
  to `ReconstructData` across randomised shard counts and missing-shard
  combinations, on both backends.
- **Budget.** With a budget forcing multiple chunk passes, output must stay
  identical to the single-pass result.
- **Memory.** A regression test asserting peak RSS stays under a bound for a
  synthetic set substantially larger than that bound.
- **Scan.** Verify reports the same `ShardCounts` as today for intact, corrupt,
  missing, and misaligned sets.
- **Regression.** The existing `par2` suite and the `realtool` fixtures
  (generated by real par2cmdline) stay green under `-race` and with
  `CGO_ENABLED=0`.
- **End to end.** `bench/run.py` on the real release confirms the projected
  memory drop and no correctness regression (all files MD5-match pristine).

## Expected outcome

On the full 4.36 GiB release: repair peak RSS ~13.7 GB to roughly 500 MB
(210 missing shards x 2.38 MB), verify to tens of MB, with unchanged results.
