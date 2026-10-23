# Repair CPU measurements — 2026-10-23

Historical worker/solver experiments against `b38267b`. The subsequent metadata
and output-write improvements are measured in [RESULTS.md](RESULTS.md).

Reducing repair concurrency can save CPU, but it does not preserve elapsed time
uniformly. No tested worker cap stayed within 5% of the 10-worker elapsed-time
baseline across all four cases. These experiments left the repair engine and worker count unchanged.
They did not complete the earlier parfast parity goal.

Apple M4, 10 logical CPUs, darwin/arm64, Go 1.27.0, native CLMul(SHA3), warm
APFS, 512 MiB default bulk memory budget. Baseline commit: `b38267b`.
Fixtures contain 10,000 blocks with 8,192 recovery blocks. The small fixture
has 16 KiB blocks (156.25 MiB data); the large fixture has 128 KiB blocks
(1.221 GiB data). Damage seed 1337 and SHA-256 validation match RESULTS.md.
Each run uses a fresh clone and a separate process. Staging and validation are
outside timing. No concurrent builds/tests ran during measurements.

Tables show medians of three repetitions, alternating execution order.
CPU-seconds are process user plus system CPU time across all threads, including
startup. They measure total CPU work, not peak utilization or energy. Small
percentage differences remain subject to run-to-run variation.

## 8 versus 10 workers

| Fixture | Missing | 10-worker time | 8-worker time | 10-worker CPU-s | 8-worker CPU-s | Time change | CPU change |
|---|---:|---:|---:|---:|---:|---:|---:|
| small | 2,048 | 0.600 s | 0.581 s | 3.639 | 3.098 | -3.2% | -14.9% |
| small | 8,192 | 1.223 s | 1.268 s | 7.855 | 6.587 | +3.7% | -16.1% |
| large | 2,048 | 2.684 s | 2.854 s | 20.199 | 18.651 | +6.3% | -7.7% |
| large | 8,192 | 6.345 s | 6.380 s | 40.725 | 35.761 | +0.5% | -12.2% |

## 9 versus 10 workers

| Fixture | Missing | 10-worker time | 9-worker time | 10-worker CPU-s | 9-worker CPU-s | Time change | CPU change |
|---|---:|---:|---:|---:|---:|---:|---:|
| small | 2,048 | 0.569 s | 0.557 s | 3.347 | 3.270 | -2.2% | -2.3% |
| small | 8,192 | 1.211 s | 1.214 s | 7.640 | 7.245 | +0.3% | -5.2% |
| large | 2,048 | 2.703 s | 2.687 s | 20.147 | 19.398 | -0.6% | -3.7% |
| large | 8,192 | 5.820 s | 6.182 s | 40.102 | 37.346 | +6.2% | -6.9% |

## Decision and remaining work

Eight workers save approximately 8–16% CPU-seconds in this sweep. Three cases
stay within 5% of baseline elapsed time, but the large/2,048-missing case is
about 6% slower. Nine workers save approximately 2–7%, but the large/8,192-missing
case is about 6% slower. Four and six workers produce larger CPU reductions
with larger slowdowns on the large fixture. These measurements support tuning
per workload, not a portable default cap.

The existing `par2.RepairOptions.NumGoroutines` provides the control; callers
can opt into a measured worker count without an API change. Compare on the
actual CPU/storage and release sizes before choosing it. A strict zero-slowdown
CPU reduction across the measured workloads remains unproven.

Four code experiments were tested and reverted because total CPU savings were
not consistent: flattening more operation batches, preparing directly from the
parent input bank, simplifying butterflies with known-zero operands, and
coalescing scale operations/special-casing coefficient-one butterflies. Native
and pure-Go solver checks passed during these experiments; arithmetic and buffer
lifetime reviews found no correctness issues in the reviewed candidates. These
rejected candidates are not part of the final repair engine.

The changes retained from this investigation were measurement tooling: CPU time and average-core metrics,
worker sweeps, alternating baseline/candidate binary comparisons, and rejection
of nonempty output directories to prevent mixed samples. See [README.md](README.md)
for commands. Future solver changes must improve CPU work while retaining repair
speed and checksum correctness; reducing worker count alone does not establish
that result.

All 168 recorded experimental repairs passed SHA-256 validation. Exact arguments,
binary/fixture hashes, per-run CPU/wall/RSS metrics and rejected experiment samples
are preserved in [cpu-results-2026-10-23.json](cpu-results-2026-10-23.json).
