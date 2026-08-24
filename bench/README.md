# PAR2 verify/repair benchmark harness

End-to-end benchmark of `gopar-turbo` against `par2cmdline-turbo` (and stock
`par2cmdline` as a floor) on a real PAR2 set, measuring wall time, peak RSS,
and repair correctness.

This measures the **whole verify/repair path** — packet parsing, MD5/CRC
scanning, matrix inversion, and the GF(2^16) recovery loop — not just the SIMD
kernel. For kernel-level throughput see `go test ./rsec16/ -bench .`.

## Pieces

| File | Role |
|---|---|
| `../cmd/par2bench` | Times one `par2.Verify`/`par2.Repair` call, emits JSON (time + peak RSS). One process per measured op, so RSS is attributable. |
| `damage.py` | Deterministic damage: delete whole files, or clobber N distinct slices. Pure function of (mode, seed, slice size, file list). |
| `run.py` | Driver: stage → damage → warm cache → time → MD5-verify, across every tool/scenario/rep. Appends to `results.csv`. |

## Scenarios

| Scenario | Damage | What it exercises |
|---|---|---|
| `verify-intact` | none | Full-set MD5/CRC32 scan, the common no-op case |
| `verify-damaged` | N corrupted slices | Scan plus slice-level match/miss bookkeeping |
| `repair-missing` | K whole files deleted | Reconstruction at file granularity |
| `repair-corrupt` | N corrupted slices | Reconstruction at slice granularity, scattered across files |

## Method

- Every measured run starts from a fresh **APFS copy-on-write clone** of the
  pristine set, so staging never lands on the clock and no run inherits a
  previous run's repairs.
- Damage is seeded and byte-identical across tools — every tool solves the
  exact same problem.
- The set is read through the page cache immediately before each timed run, so
  results reflect a **warm cache**. This deliberately factors out cold-read
  disk time; it also means these numbers are an upper bound on what you'd see
  on a cold set.
- Peak RSS comes from `os.wait4()` rusage for that specific child process.
- After every repair, all data files are MD5-compared against the pristine
  baseline. A repair that finishes fast but produces wrong bytes shows up as
  `md5=MISMATCH`, not as a win.

## A caveat about "pristine"

The driver treats `--pristine` as ground truth and MD5-compares repaired output
against it. Confirm the set actually verifies clean before trusting a run — a
real Usenet download often arrives with article loss, and a baseline taken from
damaged files scores every *correct* repair as a mismatch:

```bash
par2 verify pristine/recovery.par2   # must say repair is not required
```

Backup files are already excluded from the comparison: par2cmdline renames a
damaged file to `<name>.1` before rewriting it, and counting those as set
members produced the same false mismatch.

## Running it

```bash
go build -o /tmp/par2bench-cgo ./cmd/par2bench/
CGO_ENABLED=0 go build -o /tmp/par2bench-pure ./cmd/par2bench/

python3 bench/run.py \
  --dataset my-set --pristine /path/to/pristine --index recovery.par2 \
  --work /tmp/bench-work --results /tmp/bench-results \
  --gopar-cgo /tmp/par2bench-cgo --gopar-pure /tmp/par2bench-pure \
  --par2-turbo /path/to/par2cmdline-turbo/par2 \
  --slice-size 2380956 --reps 3 --missing-files 5 --corrupt-slices 200
```

`--pristine` must contain the data files and the whole PAR2 set, and must
never be written to — the driver only ever clones out of it.

Building `par2cmdline-turbo` (autotools; needs `automake`):

```bash
git clone --depth 1 --recurse-submodules https://github.com/animetosho/par2cmdline-turbo.git
cd par2cmdline-turbo && ./automake.sh && ./configure && make -j
```
