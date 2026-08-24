# Repair memory parity with par2cmdline-turbo

Date: 2026-08-24
Status: approved, not yet implemented
Builds on: [2026-08-24-performance-parity-design.md](2026-08-24-performance-parity-design.md)

## Problem

After the performance work, gopar-turbo matches or beats par2cmdline-turbo on
wall-clock everywhere, but repair peak RSS is ~4× theirs on the full 4.36 GiB
set: 2338 MB vs ~550 MB. Heap-profiled and measured, the peak decomposes as:

| Component | bytes | why |
|---|---:|---|
| Retained parity blocks | 675 MB | `loadParityData` copies every recovery-packet body out of its volume |
| Fold accumulators | 504 MB | one prepared buffer per missing shard (210 × 2.4 MB), default budget never chunks |
| Reconstructed shards awaiting write | ~500 MB | `reconstructMissing` returns a map held until whole files are assembled and written |
| Batch banks, scan, misc | ~130 MB | |
| GC headroom | rest | proportional to the above |

Measured enablers: chunked reconstruction at a 256 MB accumulator target costs
no time (8.73s vs 8.75s unchunked; 2 passes), and +7% at 64 MB (8 passes).

## Goal

Repair peak RSS in par2cmdline-turbo's territory — target ≤ 800 MB on the
full-set benchmark scenarios — with:

- repair wall-clock within noise of the current 9.02s / 16.66s,
- output byte-identical on both backends,
- public API unchanged (`MemoryBudget` keeps its meaning: an upper bound).

Explicit non-goals: verify memory (stays ~200 MB — the parallel scan's
working set; small in absolute terms and the price of the 3× verify win);
GC knobs (`SetMemoryLimit`/GOGC are process-global and not a library's to set).

## Design

Three changes, in dependency order.

### 1. Parity blocks on demand

`walkPackets` gains the packet body's absolute file offset (new callback
parameter). `loadParityData` in retain mode stops copying bodies; it records
per exponent a location — volume path, body-data offset, length — leaving
`parityPresent` as the presence source of truth. (A recovery packet's body is
4 bytes of exponent then the block data; the recorded offset points at the
data.) The fold's parity inputs and `verifyParity`'s comparisons read blocks
from the volumes by offset through the same reader-cache pattern data shards
use. Presence mode is unchanged. Total I/O is unchanged — the blocks were
read exactly once before, and are read once per chunk pass now.

Duplicate exponents keep the first location seen (byte-identical by packet
hash). A volume that disappears between load and fold surfaces as a read
error from the fold, which is already a handled path.

### 2. Accumulator cap

`chunkSizeFor` targets `min(MemoryBudget, 256 MB)` of accumulator bytes.
Measured free at the resulting 2 passes on the benchmark set. `MemoryBudget`
remains an upper bound the user can lower; raising it above the cap no longer
buys anything, which the option's doc comment states.

### 3. Incremental chunk writes

Reconstruction stops accumulating output shards. Instead, repair pre-plans
which files need rewriting, and each chunk pass writes its reconstructed
ranges directly into those files at their final offsets — par2cmdline's
design. Concretely:

- `fileIO` gains an `OpenWrite(path string, size int64)` returning a
  positional writer (`io.WriterAt` plus close), implemented by `defaultFileIO`
  (create/truncate) and `memfs`.
- Surviving shards of rewritten files are also copied in by chunk range
  (from their recorded `foundAt` locations, or from the pre-read map that
  guards the swapped-files hazard), so a rewritten file is complete when the
  passes finish.
- The final MD5/16k verification streams each rewritten file from disk —
  the same windowed read the verify scan uses — instead of hashing an
  in-memory assembly. A hash mismatch after write is reported exactly as
  today's pre-write mismatch (an error naming the file), the file having
  already been replaced; this matches par2cmdline, which also writes before
  its final verification pass.
- The swapped-files pre-read (`preserveShardsBeforeOverwrite`) still runs
  before any file is opened for writing.

Peak resident data becomes: capped accumulators (≤256 MB) + batch banks
(~77 MB) + one chunk's outputs in flight + scan remnants — ~400–500 MB live,
~500–800 MB RSS with GC headroom.

### Behavior change to state plainly

Today a failed repair (error mid-reconstruction or hash mismatch) leaves the
damaged files untouched, because writes happened only at the end from memory.
With incremental writes, a file being repaired is truncated/rewritten as
passes complete, so an interrupted or failed repair can leave a target file
partially written. par2cmdline has the same property (it renames the damaged
original to `<name>.1` first — we do not, and adding backups is out of scope).
The `RepairResult`/error contract is unchanged; the doc comment on `Repair`
gains a sentence stating this.

## Testing

- Existing repair equivalence and realtool suites pass unchanged (the oracle:
  repaired bytes identical to pristine).
- New: after `LoadParityData`, no recovery-block bytes are retained (the
  location table is populated instead); repair with parity-on-demand equals
  repair before this change byte-for-byte.
- New: chunked (tiny budget) vs default produce identical files with the
  incremental writer.
- New: `OpenWrite` contract tests on `defaultFileIO` and `memfs` (create,
  overwrite, sparse-offset writes, close).
- New: swapped-files test still passes (cross-file source pre-read happens
  before any OpenWrite).
- Full-set re-bench: repair RSS ≤ 800 MB, times within noise, `md5=ok`
  everywhere; RESULTS.md updated.

## Expected outcome (full set, repair)

| | now | target | par2cmdline-turbo |
|---|---:|---:|---:|
| repair-missing | 9.02s / 2338 MB | ~9s / ≤800 MB | 9.00s / 562 MB |
| repair-corrupt | 16.66s / 2158 MB | ~16.7s / ≤800 MB | 18.51s / 540 MB |
