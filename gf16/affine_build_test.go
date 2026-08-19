//go:build cgo && amd64

package gf16

import "testing"

// TestAffineAVX2CompiledWithGFNI guards a build-flag regression in gf16/Makefile.
//
// gf16_affine_avx2.c is wrapped in `#if defined(__GFNI__) && defined(__AVX2__)`,
// so without -mgfni the whole unit degrades to stubs and gf16_affine_init_avx2()
// compiles to `return NULL`. That initialiser supplies the scratch tables for
// GF16_AFFINE_AVX512, GF16_AFFINE_AVX10 and GF16_AFFINE_AVX2 as well, while
// gf16_affine_available_avx512 (built with -mgfni) stays 1 -- so on any CPU with
// GFNI + AVX-512 the dispatcher picks an affine method whose scratch pointer is
// NULL and the kernel segfaults reading it at a small offset.
//
// The crash only reproduces on GFNI-capable hardware, which CI runners provide
// inconsistently, so assert the build invariant instead: it holds on every amd64
// host regardless of CPU features.
func TestAffineAVX2CompiledWithGFNI(t *testing.T) {
	if !affineAVX2BuiltIn() {
		t.Fatal("gf16_affine_avx2.c was built without -mgfni: " +
			"gf16_affine_init_avx2() returns NULL, so the affine AVX512/AVX10/AVX2 " +
			"methods will segfault on GFNI-capable CPUs (see gf16/Makefile)")
	}
}
