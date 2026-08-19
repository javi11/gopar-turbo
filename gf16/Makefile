# Makefile for building libgf16.a static library.
#
# Each SIMD variant is compiled with its own flags. The resulting object files
# are combined into a single static archive that Go links via cgo.
#
# Usage:
#   make                    # build libgf16.a
#   make clean              # remove build artifacts

CC  ?= cc
CXX ?= c++
AR  ?= ar

VENDOR  := vendor
GF16DIR := $(VENDOR)/gf16
SRCDIR  := $(VENDOR)/src
BUILDDIR := build

# Architecture detection (overridable for cross-compilation)
UNAME_M := $(shell uname -m)
ARCH ?= $(if $(filter x86_64 amd64,$(UNAME_M)),x86_64,$(if $(filter aarch64 arm64,$(UNAME_M)),aarch64,generic))

# Common flags
# -fPIC is needed on Unix for position-independent code; on Windows (MinGW) it's
# ignored but harmless. We keep it unconditionally for simplicity.
CFLAGS_COMMON   := -O3 -fPIC -std=gnu99 -Wno-unused-function -funroll-loops -I$(VENDOR)
CXXFLAGS_COMMON := -O3 -fPIC -std=c++11 -Wno-unused-function -funroll-loops -I$(VENDOR)

# We need PARPAR_INVERT_SUPPORT for mul_add to be available
DEFINES := -DPARPAR_INVERT_SUPPORT -DPARPAR_SLIM_GF16

# Test if compiler supports a given flag
check_flag = $(shell $(1) -x c /dev/null -c -o /dev/null $(2) 2>/dev/null && echo $(2))

# ============================================================================
# Source-to-object mapping with per-file SIMD flags
# ============================================================================

# Generic / always-compiled sources
GENERIC_SRCS := \
	$(GF16DIR)/gf16_lookup.c \
	$(GF16DIR)/gf16_cksum_generic.c \
	$(GF16DIR)/gfmat_coeff.c \
	$(GF16DIR)/gf_add_generic.c

# x86_64 sources with their required flags
SSE2_SRCS := \
	$(GF16DIR)/gf_add_sse2.c \
	$(GF16DIR)/gf16_lookup_sse2.c \
	$(GF16DIR)/gf16_cksum_sse2.c \
	$(GF16DIR)/gf16_xor_sse2.c
SSE2_FLAGS := $(call check_flag,$(CC),-msse2)

SSSE3_SRCS := \
	$(GF16DIR)/gf16_shuffle_ssse3.c
SSSE3_FLAGS := $(call check_flag,$(CC),-mssse3)

AVX_SRCS := \
	$(GF16DIR)/gf16_shuffle_avx.c
AVX_FLAGS := $(call check_flag,$(CC),-mavx)

AVX2_SRCS := \
	$(GF16DIR)/gf_add_avx2.c \
	$(GF16DIR)/gf16_shuffle_avx2.c \
	$(GF16DIR)/gf16_cksum_avx2.c \
	$(GF16DIR)/gf16_xor_avx2.c \
	$(GF16DIR)/gf16_affine_avx2.c
AVX2_FLAGS := $(call check_flag,$(CC),-mavx2)

AVX512_SRCS := \
	$(GF16DIR)/gf_add_avx512.c \
	$(GF16DIR)/gf16_shuffle_avx512.c \
	$(GF16DIR)/gf16_cksum_avx512.c \
	$(GF16DIR)/gf16_xor_avx512.c \
	$(GF16DIR)/gf16_affine_avx512.c
AVX512_FLAGS := $(call check_flag,$(CC),-mavx512vl -mavx512bw)

VBMI_SRCS := \
	$(GF16DIR)/gf16_shuffle_vbmi.c
VBMI_FLAGS := $(call check_flag,$(CC),-mavx512vl -mavx512vbmi)

GFNI_SRCS := \
	$(GF16DIR)/gf16_affine_gfni.c
GFNI_FLAGS := $(call check_flag,$(CC),-mgfni -mssse3)

AVX10_SRCS := \
	$(GF16DIR)/gf_add_avx10.c \
	$(GF16DIR)/gf16_affine_avx10.c
AVX10_FLAGS := $(call check_flag,$(CC),-mgfni -mavx512vl -mavx512bw -mno-evex512)

# aarch64 sources
NEON_SRCS := \
	$(GF16DIR)/gf_add_neon.c \
	$(GF16DIR)/gf16_shuffle_neon.c \
	$(GF16DIR)/gf16_cksum_neon.c \
	$(GF16DIR)/gf16_clmul_neon.c

SHA3_SRCS := \
	$(GF16DIR)/gf16_clmul_sha3.c
SHA3_FLAGS := $(call check_flag,$(CC),-march=armv8.2-a+sha3)

SVE_SRCS := \
	$(GF16DIR)/gf_add_sve.c \
	$(GF16DIR)/gf16_shuffle128_sve.c \
	$(GF16DIR)/gf16_cksum_sve.c
SVE_FLAGS := $(call check_flag,$(CC),-march=armv8-a+sve)

SVE2_SRCS := \
	$(GF16DIR)/gf_add_sve2.c \
	$(GF16DIR)/gf16_shuffle128_sve2.c \
	$(GF16DIR)/gf16_shuffle2x128_sve2.c \
	$(GF16DIR)/gf16_shuffle512_sve2.c \
	$(GF16DIR)/gf16_clmul_sve2.c
SVE2_FLAGS := $(call check_flag,$(CC),-march=armv8-a+sve2)

# RISC-V sources
RVV_SRCS := \
	$(GF16DIR)/gf_add_rvv.c \
	$(GF16DIR)/gf16_shuffle128_rvv.c \
	$(GF16DIR)/gf16_cksum_rvv.c
RVV_FLAGS := $(call check_flag,$(CC),-march=rv64gcv)

RVV_ZVBC_SRCS := \
	$(GF16DIR)/gf16_clmul_rvv.c
RVV_ZVBC_FLAGS := $(call check_flag,$(CC),-march=rv64gcv_zvbc1)

# C++ sources
CXX_SRCS := \
	$(GF16DIR)/gf16mul.cpp \
	csrc/bridge.cpp

# ============================================================================
# Build architecture-specific object lists
# ============================================================================

ALL_OBJS :=

# Generic objects (always built)
GENERIC_OBJS := $(patsubst $(VENDOR)/%,$(BUILDDIR)/%.o,$(GENERIC_SRCS))
ALL_OBJS += $(GENERIC_OBJS)

# C++ objects (always built)
CXX_OBJS := \
	$(BUILDDIR)/gf16mul.cpp.o \
	$(BUILDDIR)/bridge.cpp.o
ALL_OBJS += $(CXX_OBJS)

ifeq ($(ARCH),x86_64)

# On x86_64, always compile ALL variant sources. When the compiler doesn't
# support a flag set, the source files detect this via preprocessor checks
# (e.g. __AVX2__, __GFNI__) and emit stub functions instead, which the
# linker needs to resolve references in gf16mul.cpp.

SSE2_OBJS := $(patsubst $(VENDOR)/%,$(BUILDDIR)/%.o,$(SSE2_SRCS))
ALL_OBJS += $(SSE2_OBJS)

SSSE3_OBJS := $(patsubst $(VENDOR)/%,$(BUILDDIR)/%.o,$(SSSE3_SRCS))
ALL_OBJS += $(SSSE3_OBJS)

AVX_OBJS := $(patsubst $(VENDOR)/%,$(BUILDDIR)/%.o,$(AVX_SRCS))
ALL_OBJS += $(AVX_OBJS)

AVX2_OBJS := $(patsubst $(VENDOR)/%,$(BUILDDIR)/%.o,$(AVX2_SRCS))
ALL_OBJS += $(AVX2_OBJS)

AVX512_OBJS := $(patsubst $(VENDOR)/%,$(BUILDDIR)/%.o,$(AVX512_SRCS))
ALL_OBJS += $(AVX512_OBJS)

VBMI_OBJS := $(patsubst $(VENDOR)/%,$(BUILDDIR)/%.o,$(VBMI_SRCS))
ALL_OBJS += $(VBMI_OBJS)

GFNI_OBJS := $(patsubst $(VENDOR)/%,$(BUILDDIR)/%.o,$(GFNI_SRCS))
ALL_OBJS += $(GFNI_OBJS)

AVX10_OBJS := $(patsubst $(VENDOR)/%,$(BUILDDIR)/%.o,$(AVX10_SRCS))
ALL_OBJS += $(AVX10_OBJS)

else ifeq ($(ARCH),aarch64)

NEON_OBJS := $(patsubst $(VENDOR)/%,$(BUILDDIR)/%.o,$(NEON_SRCS))
ALL_OBJS += $(NEON_OBJS)

ifneq ($(SHA3_FLAGS),)
SHA3_OBJS := $(patsubst $(VENDOR)/%,$(BUILDDIR)/%.o,$(SHA3_SRCS))
ALL_OBJS += $(SHA3_OBJS)
endif

ifneq ($(SVE_FLAGS),)
SVE_OBJS := $(patsubst $(VENDOR)/%,$(BUILDDIR)/%.o,$(SVE_SRCS))
ALL_OBJS += $(SVE_OBJS)
endif

ifneq ($(SVE2_FLAGS),)
SVE2_OBJS := $(patsubst $(VENDOR)/%,$(BUILDDIR)/%.o,$(SVE2_SRCS))
ALL_OBJS += $(SVE2_OBJS)
endif

endif # ARCH

# ============================================================================
# Targets
# ============================================================================

.DEFAULT_GOAL := libgf16.a

libgf16.a: $(ALL_OBJS)
	$(AR) rcs $@ $^

# Create build directories
$(BUILDDIR)/gf16:
	mkdir -p $@

# ============================================================================
# Generic C compilation rule
# ============================================================================

$(BUILDDIR)/%.c.o: $(VENDOR)/%.c | $(BUILDDIR)/gf16
	$(CC) $(CFLAGS_COMMON) $(DEFINES) -c -o $@ $<

# ============================================================================
# Per-variant C compilation rules (override the generic rule via explicit targets)
# ============================================================================

# SSE2
$(BUILDDIR)/gf16/gf_add_sse2.c.o: $(GF16DIR)/gf_add_sse2.c | $(BUILDDIR)/gf16
	$(CC) $(CFLAGS_COMMON) $(DEFINES) $(SSE2_FLAGS) -c -o $@ $<
$(BUILDDIR)/gf16/gf16_lookup_sse2.c.o: $(GF16DIR)/gf16_lookup_sse2.c | $(BUILDDIR)/gf16
	$(CC) $(CFLAGS_COMMON) $(DEFINES) $(SSE2_FLAGS) -c -o $@ $<
$(BUILDDIR)/gf16/gf16_cksum_sse2.c.o: $(GF16DIR)/gf16_cksum_sse2.c | $(BUILDDIR)/gf16
	$(CC) $(CFLAGS_COMMON) $(DEFINES) $(SSE2_FLAGS) -c -o $@ $<
$(BUILDDIR)/gf16/gf16_xor_sse2.c.o: $(GF16DIR)/gf16_xor_sse2.c | $(BUILDDIR)/gf16
	$(CC) $(CFLAGS_COMMON) $(DEFINES) $(SSE2_FLAGS) -c -o $@ $<

# SSSE3
$(BUILDDIR)/gf16/gf16_shuffle_ssse3.c.o: $(GF16DIR)/gf16_shuffle_ssse3.c | $(BUILDDIR)/gf16
	$(CC) $(CFLAGS_COMMON) $(DEFINES) $(SSSE3_FLAGS) -c -o $@ $<

# AVX
$(BUILDDIR)/gf16/gf16_shuffle_avx.c.o: $(GF16DIR)/gf16_shuffle_avx.c | $(BUILDDIR)/gf16
	$(CC) $(CFLAGS_COMMON) $(DEFINES) $(AVX_FLAGS) -c -o $@ $<

# AVX2
$(BUILDDIR)/gf16/gf_add_avx2.c.o: $(GF16DIR)/gf_add_avx2.c | $(BUILDDIR)/gf16
	$(CC) $(CFLAGS_COMMON) $(DEFINES) $(AVX2_FLAGS) -c -o $@ $<
$(BUILDDIR)/gf16/gf16_shuffle_avx2.c.o: $(GF16DIR)/gf16_shuffle_avx2.c | $(BUILDDIR)/gf16
	$(CC) $(CFLAGS_COMMON) $(DEFINES) $(AVX2_FLAGS) -c -o $@ $<
$(BUILDDIR)/gf16/gf16_cksum_avx2.c.o: $(GF16DIR)/gf16_cksum_avx2.c | $(BUILDDIR)/gf16
	$(CC) $(CFLAGS_COMMON) $(DEFINES) $(AVX2_FLAGS) -c -o $@ $<
$(BUILDDIR)/gf16/gf16_xor_avx2.c.o: $(GF16DIR)/gf16_xor_avx2.c | $(BUILDDIR)/gf16
	$(CC) $(CFLAGS_COMMON) $(DEFINES) $(AVX2_FLAGS) -c -o $@ $<
# Needs -mgfni as well as -mavx2: the whole translation unit is guarded by
# `#if defined(__GFNI__) && defined(__AVX2__)`, and gf16_affine_init_avx2()
# compiles to `return NULL` without it. That initialiser supplies the scratch
# tables for GF16_AFFINE_AVX512/AVX10/AVX2 too, so building it as a stub makes
# those methods segfault on any GFNI-capable CPU. Mirrors the avx512 rule below.
$(BUILDDIR)/gf16/gf16_affine_avx2.c.o: $(GF16DIR)/gf16_affine_avx2.c | $(BUILDDIR)/gf16
	$(CC) $(CFLAGS_COMMON) $(DEFINES) -mgfni $(AVX2_FLAGS) -c -o $@ $<

# AVX-512
$(BUILDDIR)/gf16/gf_add_avx512.c.o: $(GF16DIR)/gf_add_avx512.c | $(BUILDDIR)/gf16
	$(CC) $(CFLAGS_COMMON) $(DEFINES) $(AVX512_FLAGS) -c -o $@ $<
$(BUILDDIR)/gf16/gf16_shuffle_avx512.c.o: $(GF16DIR)/gf16_shuffle_avx512.c | $(BUILDDIR)/gf16
	$(CC) $(CFLAGS_COMMON) $(DEFINES) $(AVX512_FLAGS) -c -o $@ $<
$(BUILDDIR)/gf16/gf16_cksum_avx512.c.o: $(GF16DIR)/gf16_cksum_avx512.c | $(BUILDDIR)/gf16
	$(CC) $(CFLAGS_COMMON) $(DEFINES) $(AVX512_FLAGS) -c -o $@ $<
$(BUILDDIR)/gf16/gf16_xor_avx512.c.o: $(GF16DIR)/gf16_xor_avx512.c | $(BUILDDIR)/gf16
	$(CC) $(CFLAGS_COMMON) $(DEFINES) $(AVX512_FLAGS) -c -o $@ $<
$(BUILDDIR)/gf16/gf16_affine_avx512.c.o: $(GF16DIR)/gf16_affine_avx512.c | $(BUILDDIR)/gf16
	$(CC) $(CFLAGS_COMMON) $(DEFINES) -mgfni $(AVX512_FLAGS) -c -o $@ $<

# VBMI
$(BUILDDIR)/gf16/gf16_shuffle_vbmi.c.o: $(GF16DIR)/gf16_shuffle_vbmi.c | $(BUILDDIR)/gf16
	$(CC) $(CFLAGS_COMMON) $(DEFINES) $(VBMI_FLAGS) -c -o $@ $<

# GFNI
$(BUILDDIR)/gf16/gf16_affine_gfni.c.o: $(GF16DIR)/gf16_affine_gfni.c | $(BUILDDIR)/gf16
	$(CC) $(CFLAGS_COMMON) $(DEFINES) $(GFNI_FLAGS) -c -o $@ $<

# AVX10
$(BUILDDIR)/gf16/gf_add_avx10.c.o: $(GF16DIR)/gf_add_avx10.c | $(BUILDDIR)/gf16
	$(CC) $(CFLAGS_COMMON) $(DEFINES) $(AVX10_FLAGS) -c -o $@ $<
$(BUILDDIR)/gf16/gf16_affine_avx10.c.o: $(GF16DIR)/gf16_affine_avx10.c | $(BUILDDIR)/gf16
	$(CC) $(CFLAGS_COMMON) $(DEFINES) $(AVX10_FLAGS) -c -o $@ $<

# NEON (aarch64 - no extra flags needed, NEON is always available)
$(BUILDDIR)/gf16/gf_add_neon.c.o: $(GF16DIR)/gf_add_neon.c | $(BUILDDIR)/gf16
	$(CC) $(CFLAGS_COMMON) $(DEFINES) -c -o $@ $<
$(BUILDDIR)/gf16/gf16_shuffle_neon.c.o: $(GF16DIR)/gf16_shuffle_neon.c | $(BUILDDIR)/gf16
	$(CC) $(CFLAGS_COMMON) $(DEFINES) -c -o $@ $<
$(BUILDDIR)/gf16/gf16_cksum_neon.c.o: $(GF16DIR)/gf16_cksum_neon.c | $(BUILDDIR)/gf16
	$(CC) $(CFLAGS_COMMON) $(DEFINES) -c -o $@ $<
$(BUILDDIR)/gf16/gf16_clmul_neon.c.o: $(GF16DIR)/gf16_clmul_neon.c | $(BUILDDIR)/gf16
	$(CC) $(CFLAGS_COMMON) $(DEFINES) -c -o $@ $<

# SHA3
$(BUILDDIR)/gf16/gf16_clmul_sha3.c.o: $(GF16DIR)/gf16_clmul_sha3.c | $(BUILDDIR)/gf16
	$(CC) $(CFLAGS_COMMON) $(DEFINES) $(SHA3_FLAGS) -c -o $@ $<

# SVE
$(BUILDDIR)/gf16/gf_add_sve.c.o: $(GF16DIR)/gf_add_sve.c | $(BUILDDIR)/gf16
	$(CC) $(CFLAGS_COMMON) $(DEFINES) $(SVE_FLAGS) -c -o $@ $<
$(BUILDDIR)/gf16/gf16_shuffle128_sve.c.o: $(GF16DIR)/gf16_shuffle128_sve.c | $(BUILDDIR)/gf16
	$(CC) $(CFLAGS_COMMON) $(DEFINES) $(SVE_FLAGS) -c -o $@ $<
$(BUILDDIR)/gf16/gf16_cksum_sve.c.o: $(GF16DIR)/gf16_cksum_sve.c | $(BUILDDIR)/gf16
	$(CC) $(CFLAGS_COMMON) $(DEFINES) $(SVE_FLAGS) -c -o $@ $<

# SVE2
$(BUILDDIR)/gf16/gf_add_sve2.c.o: $(GF16DIR)/gf_add_sve2.c | $(BUILDDIR)/gf16
	$(CC) $(CFLAGS_COMMON) $(DEFINES) $(SVE2_FLAGS) -c -o $@ $<
$(BUILDDIR)/gf16/gf16_shuffle128_sve2.c.o: $(GF16DIR)/gf16_shuffle128_sve2.c | $(BUILDDIR)/gf16
	$(CC) $(CFLAGS_COMMON) $(DEFINES) $(SVE2_FLAGS) -c -o $@ $<
$(BUILDDIR)/gf16/gf16_shuffle2x128_sve2.c.o: $(GF16DIR)/gf16_shuffle2x128_sve2.c | $(BUILDDIR)/gf16
	$(CC) $(CFLAGS_COMMON) $(DEFINES) $(SVE2_FLAGS) -c -o $@ $<
$(BUILDDIR)/gf16/gf16_shuffle512_sve2.c.o: $(GF16DIR)/gf16_shuffle512_sve2.c | $(BUILDDIR)/gf16
	$(CC) $(CFLAGS_COMMON) $(DEFINES) $(SVE2_FLAGS) -c -o $@ $<
$(BUILDDIR)/gf16/gf16_clmul_sve2.c.o: $(GF16DIR)/gf16_clmul_sve2.c | $(BUILDDIR)/gf16
	$(CC) $(CFLAGS_COMMON) $(DEFINES) $(SVE2_FLAGS) -c -o $@ $<

# RVV
$(BUILDDIR)/gf16/gf_add_rvv.c.o: $(GF16DIR)/gf_add_rvv.c | $(BUILDDIR)/gf16
	$(CC) $(CFLAGS_COMMON) $(DEFINES) $(RVV_FLAGS) -c -o $@ $<
$(BUILDDIR)/gf16/gf16_shuffle128_rvv.c.o: $(GF16DIR)/gf16_shuffle128_rvv.c | $(BUILDDIR)/gf16
	$(CC) $(CFLAGS_COMMON) $(DEFINES) $(RVV_FLAGS) -c -o $@ $<
$(BUILDDIR)/gf16/gf16_cksum_rvv.c.o: $(GF16DIR)/gf16_cksum_rvv.c | $(BUILDDIR)/gf16
	$(CC) $(CFLAGS_COMMON) $(DEFINES) $(RVV_FLAGS) -c -o $@ $<
$(BUILDDIR)/gf16/gf16_clmul_rvv.c.o: $(GF16DIR)/gf16_clmul_rvv.c | $(BUILDDIR)/gf16
	$(CC) $(CFLAGS_COMMON) $(DEFINES) $(RVV_ZVBC_FLAGS) -c -o $@ $<

# C++ sources
$(BUILDDIR)/gf16mul.cpp.o: $(GF16DIR)/gf16mul.cpp | $(BUILDDIR)/gf16
	$(CXX) $(CXXFLAGS_COMMON) $(DEFINES) -c -o $@ $<

$(BUILDDIR)/bridge.cpp.o: csrc/bridge.cpp | $(BUILDDIR)/gf16
	$(CXX) $(CXXFLAGS_COMMON) $(DEFINES) -I. -c -o $@ $<

# ============================================================================
# Housekeeping
# ============================================================================

# ============================================================================
# Platform targets (for pre-building committed .a files)
# ============================================================================

# Build for the current platform and copy to the native-named output
.PHONY: native
native: libgf16.a
ifeq ($(shell uname -s),Darwin)
	cp libgf16.a libgf16_darwin.a
else ifeq ($(shell uname -s),Linux)
  ifeq ($(ARCH),x86_64)
	cp libgf16.a libgf16_linux_amd64.a
  else
	cp libgf16.a libgf16_linux_arm64.a
  endif
else
	cp libgf16.a libgf16_windows_amd64.a
endif

# macOS universal binary (amd64 + arm64)
.PHONY: darwin
darwin:
	$(MAKE) clean
	$(MAKE) ARCH=x86_64 CC="cc -target x86_64-apple-macos11" CXX="c++ -target x86_64-apple-macos11" libgf16.a
	mv libgf16.a libgf16_x86_64.a
	$(MAKE) clean
	$(MAKE) ARCH=aarch64 libgf16.a
	mv libgf16.a libgf16_arm64.a
	lipo -create libgf16_x86_64.a libgf16_arm64.a -output libgf16_darwin.a
	rm -f libgf16_x86_64.a libgf16_arm64.a

# Linux amd64 (run on ubuntu-latest)
.PHONY: linux/amd64
linux/amd64: libgf16.a
	cp libgf16.a libgf16_linux_amd64.a

# Linux arm64 (run on ubuntu-24.04-arm)
.PHONY: linux/arm64
linux/arm64: libgf16.a
	cp libgf16.a libgf16_linux_arm64.a

# Windows amd64 (run on windows-latest with MSYS2/MinGW)
.PHONY: windows/amd64
windows/amd64: libgf16.a
	cp libgf16.a libgf16_windows_amd64.a

.PHONY: clean
clean:
	rm -rf $(BUILDDIR) libgf16.a
