# Deep repair implementation plan

> Execution: implement inline with test-first changes and independent review.

Goal: reduce deep repair cost through structured PAR2 planning and a measured additive-field transform path.
Spec: ../specs/2026-10-21-deep-repair-design.md.

- [x] Pin baseline comparisons to downloaded parfast 1.7.1, exact seeded damage and SHA-256 output validation. Preserve earlier uncommitted performance work.
- [x] Add closed-form consecutive-exponent reconstruction coefficients, validated against Gaussian elimination for offset exponents, reordered rows, all-missing and irregular fallback cases; benchmark planning independently.
- [x] Implement scalar additive basis/FFT, derivative and product operations. Validate every operation against elementary polynomial arithmetic before vectorization.
- [x] Add batched SIMD region operations with explicit buffer pinning and pure-Go support; validate all operations, alias rules, errors and concurrent disjoint work.
- [x] Compose bounded vector reconstruction from Cauchy and polynomial-product identities. Compare exact bytes against Gaussian elimination and independent PAR2 fixtures. Keep irregular exponents on the existing path.
- [x] Integrate a reconstruction plan into disk streaming without allocating dense matrices on the transform path; account workspace and honor budgets/read failures.
- [x] Re-run fixed benchmark ladder and enable only measured winning regimes. Run both full suites, race checks, vet and independent code review. Record exact conditions and remaining gaps.

## Follow-up: parity target

- [x] Replace full-field Cauchy passes with transposed degree-limited moments
  and a two-bank coset workspace.
- [x] Fuse/prune butterflies, reuse native workspaces, parallelize byte stripes,
  and remove redundant preparation/finish calls for native plain layouts.
- [x] Batch parity reads and verify densely repaired files through windows.
- [x] Account for SIMD padding, avoid stripe-count overflow, and raise the
  bounded default bulk budget to 512 MiB to reduce repeated disk passes.
- [x] Fix strict-GC failure caused by uninitialized native pointer-table slots;
  validate with repeated aggressive-GC runs and independent review.
- [x] Parallelize writes across files while waiting for all writes before
  buffer reuse or abort; test overlapping success/failure lifetimes.
- [x] Initialize generic multiplication tables lazily and validate concurrent
  initialization, native repair and the pure-Go fallback.
- [x] Finish repeated comparisons for 16 KiB and 128 KiB block fixtures.
- [x] Record final medians, memory usage and remaining performance limits.
- [ ] Close the remaining 2–24% wall-time gap on the 16 KiB fixture.

Historical full-field results remain in bench/RESULTS.md; they are superseded
by the transposed-moment implementation. The 2026-10-23 comparison validates
48 repairs and shows gopar ahead on all four 128 KiB cases, but behind on the
16 KiB cases. Matching parfast across the complete ladder remains unfinished.

Latest remaining-gap investigation: a CPU profile of the 16 KiB/2,048-missing
case attributes most sampled arithmetic CPU to native transform calls. A
1,024-byte stripe prototype did not improve the repeated small-case medians
(0.489/0.652 s versus the retained 256-byte variant's 0.480/0.604 s); all
12 experimental repairs passed SHA-256. The prototype was reverted. Further
work should profile native transform execution and memory traffic; Go's
cgocall attribution alone does not distinguish kernel work from call overhead.
