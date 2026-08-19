// bridge.h - C API over ParPar's Galois16Mul for cgo.
// A gf16_ctx wraps one Galois16Mul instance plus its mutScratch buffer.
// NOT thread-safe for gf16_mul/gf16_muladd/gf16_muladd_multi (mutScratch);
// gf16_prepare/gf16_finish only touch caller buffers and may run concurrently.
#ifndef GF16_BRIDGE_H
#define GF16_BRIDGE_H

#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

typedef struct gf16_ctx gf16_ctx_t;

// slice_size must be > 0 and even. Returns NULL on failure.
gf16_ctx_t* gf16_create(size_t slice_size);
void        gf16_destroy(gf16_ctx_t* ctx);

const char* gf16_method_name(gf16_ctx_t* ctx);
size_t      gf16_buf_size(gf16_ctx_t* ctx);   // slice_size rounded up to stride
size_t      gf16_alignment(gf16_ctx_t* ctx);  // required buffer alignment
size_t      gf16_stride(gf16_ctx_t* ctx);     // len granularity for mul ops

// Transform src (src_len <= slice_size bytes) into dst (gf16_buf_size bytes,
// aligned). Trailing bytes up to buf_size are zero-padded.
void gf16_prepare(gf16_ctx_t* ctx, void* dst, const void* src, size_t src_len);
// Untransform buf (gf16_buf_size bytes) in place back to standard layout.
void gf16_finish(gf16_ctx_t* ctx, void* buf);

// Non-zero if the GFNI/AVX2 affine translation unit was built with -mgfni.
// GF16_AFFINE_AVX512/AVX10/AVX2 all take their scratch tables from
// gf16_affine_init_avx2(), which compiles to `return NULL` without that flag,
// so a zero here means those methods segfault on GFNI-capable CPUs.
// Always 0 on non-x86 builds, where the affine methods don't exist.
int gf16_affine_avx2_built(void);

// dst/src are prepared buffers; len is a multiple of stride.
void gf16_mul(gf16_ctx_t* ctx, void* dst, const void* src, size_t len, uint16_t coeff);
void gf16_muladd(gf16_ctx_t* ctx, void* dst, const void* src, size_t len, uint16_t coeff);
// dst += sum(coeffs[i] * srcs[i]) over [offset, offset+len) of each buffer.
void gf16_muladd_multi(gf16_ctx_t* ctx, unsigned n, size_t offset, void* dst,
                       void** srcs, size_t len, const uint16_t* coeffs);

#ifdef __cplusplus
}
#endif
#endif
