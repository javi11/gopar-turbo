// bridge.cpp - implements bridge.h over Galois16Mul.
#include "bridge.h"
#include "vendor/gf16/gf16mul.h"

#include <cstring>
#include <new>

struct gf16_ctx {
    Galois16Mul* gf;
    void*        mutScratch;
    size_t       sliceSize;
    size_t       bufSize;
};

extern "C" {

gf16_ctx_t* gf16_create(size_t slice_size) {
    if (slice_size == 0 || (slice_size & 1)) return nullptr;
    gf16_ctx_t* ctx = new (std::nothrow) gf16_ctx_t;
    if (!ctx) return nullptr;
    // forInvert=true biases method choice toward arbitrary-coefficient use.
    Galois16Methods method =
        Galois16Mul::default_method(slice_size, 32768, 65535, true);
    ctx->gf = new (std::nothrow) Galois16Mul(method);
    if (!ctx->gf) { delete ctx; return nullptr; }
    ctx->mutScratch = ctx->gf->mutScratch_alloc();
    ctx->sliceSize  = slice_size;
    ctx->bufSize    = ctx->gf->alignToStride(slice_size);
    return ctx;
}

void gf16_destroy(gf16_ctx_t* ctx) {
    if (!ctx) return;
    if (ctx->gf) {
        ctx->gf->mutScratch_free(ctx->mutScratch);
        delete ctx->gf;
    }
    delete ctx;
}

const char* gf16_method_name(gf16_ctx_t* ctx) { return ctx->gf->info().name; }
size_t gf16_buf_size(gf16_ctx_t* ctx)  { return ctx->bufSize; }
size_t gf16_alignment(gf16_ctx_t* ctx) { return ctx->gf->info().alignment; }
size_t gf16_stride(gf16_ctx_t* ctx)    { return ctx->gf->info().stride; }

void gf16_prepare(gf16_ctx_t* ctx, void* dst, const void* src, size_t src_len) {
    if (src_len < ctx->bufSize)
        memset((uint8_t*)dst + src_len, 0, ctx->bufSize - src_len);
    ctx->gf->prepare(dst, src, src_len);
}

void gf16_finish(gf16_ctx_t* ctx, void* buf) {
    ctx->gf->finish(buf, ctx->bufSize);
}

void gf16_mul(gf16_ctx_t* ctx, void* dst, const void* src, size_t len, uint16_t coeff) {
    ctx->gf->mul(dst, src, len, coeff, ctx->mutScratch);
}

void gf16_muladd(gf16_ctx_t* ctx, void* dst, const void* src, size_t len, uint16_t coeff) {
    ctx->gf->mul_add(dst, src, len, coeff, ctx->mutScratch);
}

void gf16_muladd_multi(gf16_ctx_t* ctx, unsigned n, size_t offset, void* dst,
                       void** srcs, size_t len, const uint16_t* coeffs) {
    ctx->gf->mul_add_multi(n, offset, dst, (const void* const*)srcs, len,
                           coeffs, ctx->mutScratch);
}

} // extern "C"
