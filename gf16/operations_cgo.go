//go:build cgo && (darwin || (linux && (amd64 || arm64)) || (windows && amd64))

package gf16

/*
#include "bridge.h"
#include <string.h>
#include <stdlib.h>

typedef struct { uint32_t dst, src; uint16_t coeff, kind; } gf16_operation;
static void gf16_apply_operations(gf16_ctx_t* ctx, void** rows, size_t len,
 const gf16_operation* ops, size_t count, void* scratch) {
 for(size_t i=0;i<count;) {
  const gf16_operation* op=ops+i;
  size_t run=1;
  // A bank of prepared buffers is itself a valid larger prepared region.
  // Combine only contiguous, independent operations; overlapping source and
  // destination ranges must preserve their original sequential semantics.
  if(op->kind==0 || op->kind==2 || op->kind>=3) {
   while(i+run<count) {
    const gf16_operation* next=ops+i+run;
    if(next->kind!=op->kind || next->coeff!=op->coeff ||
       next->dst!=op->dst+run ||
       (uintptr_t)rows[next->dst]!=(uintptr_t)rows[op->dst]+run*len) break;
    if(op->kind==0 || op->kind>=3) {
     if(next->src!=op->src+run ||
        (uintptr_t)rows[next->src]!=(uintptr_t)rows[op->src]+run*len) break;
     uintptr_t d=(uintptr_t)rows[op->dst],s=(uintptr_t)rows[op->src];
     size_t bytes=(run+1)*len;
     if(d!=s && d<s+bytes && s<d+bytes) break;
    }
    run++;
   }
  }
  size_t bytes=run*len;
  if(op->kind==0) {
   if(op->coeff==1) {
    unsigned char* d=(unsigned char*)rows[op->dst];
    const unsigned char* s=(const unsigned char*)rows[op->src];
    for(size_t j=0;j<bytes;j++) d[j]^=s[j];
   } else if(op->coeff) gf16_muladd(ctx,rows[op->dst],rows[op->src],bytes,op->coeff);
  } else if(op->kind==1) {
   uintptr_t d=(uintptr_t)rows[op->dst],s=(uintptr_t)rows[op->src];
   // Native multiplication supports exact in-place operation. Preserve the
   // snapshot semantics of OpScale for partially overlapping buffers.
   if(d==s || d+len<=s || s+len<=d) {
    gf16_mul(ctx,rows[op->dst],rows[op->src],len,op->coeff);
   } else {
    gf16_mul(ctx,scratch,rows[op->src],len,op->coeff);
    memcpy(rows[op->dst],scratch,len);
   }
  } else if(op->kind>=3) {
   unsigned char* a=(unsigned char*)rows[op->dst];
   unsigned char* b=(unsigned char*)rows[op->src];
   if(op->kind==4) for(size_t j=0;j<bytes;j++) b[j]^=a[j];
   if(op->coeff) gf16_muladd(ctx,a,b,bytes,op->coeff);
   if(op->kind==3) for(size_t j=0;j<bytes;j++) b[j]^=a[j];
  } else {
   memset(rows[op->dst],0,bytes);
  }
  i+=run;
 }
}
*/
import "C"

import (
	"errors"
	"runtime"
	"unsafe"
)

func newBufferSet(ctx *Context, rows [][]byte) (*BufferSet, error) {
	for _, row := range rows {
		if uintptr(unsafe.Pointer(&row[0]))%uintptr(ctx.align) != 0 {
			return nil, errors.New("gf16: unaligned prepared buffer")
		}
	}
	if unsafe.Sizeof(Operation{}) != C.sizeof_gf16_operation {
		return nil, errors.New("gf16: operation layout mismatch")
	}
	pins := new(runtime.Pinner)
	// Go pointer stores execute a deletion write barrier even for this
	// C-owned array. Zero old slots so the barrier cannot shade stale
	// pointer-shaped bytes left by a previous native allocation.
	memory := C.calloc(C.size_t(len(rows)), C.size_t(unsafe.Sizeof(unsafe.Pointer(nil))))
	if memory == nil {
		return nil, errors.New("gf16: pointer bank allocation failed")
	}
	ptrs := unsafe.Slice((*unsafe.Pointer)(memory), len(rows))
	for i, row := range rows {
		pins.Pin(&row[0])
		ptrs[i] = unsafe.Pointer(&row[0])
	}
	// The pointer array is C-owned. Every referenced Go allocation remains
	// pinned until Close. Passing a Go pointer array would make cgo rescan
	// the entire 65536-buffer bank on every batch, defeating SIMD batching.
	s := &BufferSet{count: len(rows), size: ctx.BufSize(), method: ctx.MethodName(), rows: rows}
	s.apply = func(c *Context, ops []Operation) {
		scratch := c.operationScratch()
		C.gf16_apply_operations(c.ctx, (*unsafe.Pointer)(memory), C.size_t(c.bufSize),
			(*C.gf16_operation)(unsafe.Pointer(&ops[0])), C.size_t(len(ops)), unsafe.Pointer(&scratch[0]))
		runtime.KeepAlive(rows)
		runtime.KeepAlive(c)
	}
	s.close = func() { C.free(memory); pins.Unpin() }
	return s, nil
}

func newBufferWorkspace(ctx *Context, count int) (*BufferSet, error) {
	stride := (ctx.bufSize + ctx.align - 1) / ctx.align * ctx.align
	if stride > (int(^uint(0)>>1)-ctx.align)/count {
		return nil, errors.New("gf16: workspace too large")
	}
	memory := C.calloc(1, C.size_t(stride*count+ctx.align))
	if memory == nil {
		return nil, errors.New("gf16: workspace allocation failed")
	}
	address := uintptr(memory)
	offset := (uintptr(ctx.align) - address%uintptr(ctx.align)) % uintptr(ctx.align)
	bank := unsafe.Slice((*byte)(unsafe.Add(memory, offset)), stride*count)
	rows := make([][]byte, count)
	for i := range rows {
		rows[i] = bank[i*stride : i*stride+ctx.bufSize : i*stride+ctx.bufSize]
	}
	set, err := newBufferSet(ctx, rows)
	if err != nil {
		C.free(memory)
		return nil, err
	}
	closePointers := set.close
	set.close = func() {
		// Remove native addresses from Go heap objects before releasing the
		// allocation, including views returned through Buffers.
		clear(rows)
		closePointers()
		C.free(memory)
	}
	return set, nil
}
