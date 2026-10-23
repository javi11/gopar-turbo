# Repair performance implementation

User approved implementation of the measured audit findings. Preserve repair bytes, atomic output staging, public constructors and pure-Go support.

1. Add parity-check read-count regressions with forced chunking and partial final slices. Add sparse recovery repair and parity corruption tests. Read directly into chunk buffers and construct matrices for explicit recovery exponents only.
2. Add folding allocation benchmark and buffer-sizing tests. Adapt input batch size to large chunks, reuse raw buffers per reader worker, and include caller output, prepared output and input banks in repair chunk sizing. Keep a conservative default budget while honoring larger explicit budgets. The budget covers bulk fold buffers, not matrices, scan buffers, native contexts or total process RSS; document minimum/alignment overhead.
3. Validate accelerated reconstruction-matrix setup against the existing independent naive solver. Invert only the missing-block square matrix, then multiply its inverse by the wide augmented matrix using the existing parallel SIMD/pure-Go matrix application. Preserve singular-matrix errors.
4. Run full suites with and without cgo, race checks on changed packages, vet, allocation probes and representative benchmarks. Record actual measurements and remaining cold-storage validation limits.

Review focus: sparse exponent identity, zero missing shards/parity, trailing zero padding, tiny budgets, backend alignment, worker buffer ownership, error cleanup, matrix ordering and singularity.
