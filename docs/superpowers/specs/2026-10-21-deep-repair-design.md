# Deep PAR2 repair solver

Goal: reduce deep-repair runtime toward parfast while retaining the Go library API, atomic staging, exact PAR2 compatibility and bounded bulk buffers. Use the downloaded parfast binary only for differential testing; implement independently from field/polynomial identities. Keep existing matrix decoding as fallback for irregular recovery exponents and small repairs. No promise of parity until measured.

## Structured planning first

For k missing data positions x_i = generators[missing_i], k consecutive recovery exponents e0..e0+k-1 define P(t)=product(t+x_i). Its Lagrange numerators are q_i(t)=P(t)/(t+x_i), and d_i=q_i(x_i)=P'(x_i). The inverse transposed Vandermonde row is q_i's coefficients divided by d_i*x_i^e0. Surviving data coefficient at y is P(y)*y^e0 / ((y+x_i)*d_i*x_i^e0). This builds reconstruction coefficients in O(k²+kn), replacing cubic inversion. Nonconsecutive exponents use the existing solver. Check against independent Gaussian elimination and random byte repairs, including offset exponents and reordered missing/available positions.

## Transform path

For deep consecutive-exponent jobs, avoid materializing the k×n matrix. Use
the existing PAR2 field (polynomial 0x1100b), with Cantor coordinates and explicit
maps between coordinates and field elements. Let m=nextPow2(k).

The ordinary-to-novel coefficient conversion is N, and degree-limited novel
evaluation across all field points is E. Then the Vandermonde evaluation is
V=EN. Its transpose computes the survivor moments as NᵀEᵀw, where a surviving
block at y has weight w_y=data_y*y^e0.

1. Process the field in m-point cosets. Scatter weighted survivors into one
   m-row bank, run the transposed local evaluation, and XOR into a second
   m-row accumulation bank. Prune butterflies whose inputs are zero. Preserve
   each coset's global offset when calculating twiddle factors.
2. Apply transposed coefficient conversion to the accumulated moments.
   Subtract (XOR) the first k moments from the k recovery blocks.
3. Reverse exactly those k residual coefficients to form R(t). Multiply by
   P(t)=product(t+x_i), using an additive FFT of length nextPow2(2k). Extract
   coefficients k..2k-1 and shift them down to obtain Q(t).
4. Convert Q to novel coefficients. Keep one copy in the second bank, evaluate
   each needed coset in the first bank, and divide each missing value by
   P'(x_i)*x_i^e0. Prune outputs that are not requested.

The forward butterfly F=[[1,c],[1,c+1]] obeys Fᵀ=J F⁻¹ J, with J exchanging
the two coordinates. Transpose reverses the stage order as well. Conversion
transposition similarly reverses operation order and exchanges source and
destination of XOR additions.

Bulk workspace is 2m prepared rows per worker. Independent 256-byte stripes
run on a bounded worker pool, sharing immutable scalar/operation plans and a
single raw input cache. Inputs are read once per outer byte range. Budgeting
uses actual backend-padded row sizes, raw inputs and caller outputs. Default
bulk budget is 512 MiB (also capped at half physical RAM by the disk API);
explicit smaller/larger budgets remain authoritative. Plans and scan buffers
are additional to this budget, which is not a process RSS limit. Multi-pass chunks are rounded down to
4 KiB boundaries when possible to reduce overlapping positional writes.

SIMD operations are batched across cgo, with fused forward/inverse butterflies,
native-owned workspaces, pinned external buffers and a pure-Go equivalent.
Workspace reuse and Close must also work when chunk sizes or worker counts
change. The previous full-field Cauchy/derivative implementation is superseded
by this degree-limited transposed-moment path.

## Validation

Test basis conversion round trips, FFT versus direct evaluation, inverse FFT, derivative versus ordinary polynomial differentiation, Cauchy product versus direct division, vector polynomial product versus schoolbook, and full recovery versus Gaussian elimination. Test zero/one blocks, partial final slices, nonzero recovery exponent starts, irregular exponents falling back, SIMD versus pure Go, workspace limits and read failures. Benchmark 10000 × 16KiB data blocks with 1024,2048,4096,8192 missing, same seeded damage, warm-cache staging and SHA-256 correctness against current parfast 1.7.1. Report losses as well as wins. Cold storage remains separate validation.

## Implementation rulings

- Use a Cantor coordinate basis inside the same PAR2 field. Choose beta_15 with field trace one and beta_(i-1)=beta_i²+beta_i. Then s_i=L^i for L(x)=x²+x, all s_i(beta_i)=s_i'(0)=1, and coefficient conversion/differentiation need only XOR. Explicit coordinate maps preserve field-element identities. This replaces the initial binary-coordinate choice without changing arithmetic or stored PAR2 format.
- Retain matrix fallback for irregular exponent sets and small repairs. Benchmark changes against both correctness and complete repair wall time; native kernel experiments that do not improve that measurement are discarded.

## Native lifetime and I/O validation

Native pointer tables are allocated with calloc before Go stores pointers into
them. Uninitialized malloc slots may contain stale Go addresses, which Go's
deletion write barrier can inspect during the store. Pinned external buffers
remain live through Close; owned row views are cleared before native release.
Strict cgo checking plus aggressive garbage collection and repeated registration
exercise this boundary.

Parity validation reads contiguous batches of at most 1 MiB, except when a
single validated packet is larger. Each packet still receives its own header
and hash validation. Borrowed packet bodies are consumed before buffer reuse;
the ordinary packet parser continues to return an owned copy. Dense repaired
files use sequential window verification, while sparse repairs retain
per-slice verification.

Reconstructed writes run concurrently across files, with sequential writes
within each file and a bounded worker count. Every write completes before the
chunk buffers can be reused or an error can close the writers. A regression
test forces one writer to fail while another remains active and checks that
cleanup waits and no file is committed.

Generic GF16 region tables initialize each coefficient on demand using
sync.Once. This avoids touching the entire 64 MiB table at process startup
when native arithmetic handles the bulk work. Concurrent callers see only
complete entries; scalar field tables and the amd64 SSSE3 table retain their
existing initialization. Native and pure-Go suites exercise both paths.
