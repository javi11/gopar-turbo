// The pre-built libgf16 archives for Linux and Windows are compiled with GCC,
// so gf16mul.cpp's std::vector instantiation references the libstdc++-internal
// helper std::__throw_length_error. Consumers that cross-compile with a
// libc++-based toolchain (zig cc, clang -stdlib=libc++) have no such symbol and
// fail to link.
//
// This translation unit is packaged as its own archive, listed AFTER -lstdc++
// in the cgo LDFLAGS, so the linker only extracts it when the C++ runtime did
// not already provide the symbol. libstdc++ builds are therefore unaffected.
//
// Aborting rather than throwing keeps the object free of any C++ runtime
// dependency of its own. The only caller is std::vector's max_size() check for
// a vector of enum values, which cannot trigger in practice.

#include <stdlib.h>

namespace std {
__attribute__((noreturn)) void __throw_length_error(const char *);
__attribute__((noreturn)) void __throw_length_error(const char *) { abort(); }
}
