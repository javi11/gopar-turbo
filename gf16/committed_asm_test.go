package gf16

import (
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"
)

// surrogateMarker is stamped by asmgen/x86.go -surrogate on its XOR-for-
// GF2P8AFFINEQB development output (see asmgen/x86.go).
const surrogateMarker = "GF16_SURROGATE_OUTPUT"

// TestCommittedAffineAssembly guards the GFNI kernels against a committed surrogate file, whose products
// would be silently wrong. It reads the files as text, so it runs on every
// host.
func TestCommittedAffineAssembly(t *testing.T) {
	for _, name := range []string{"affine_amd64.s", "shuffle_amd64.s"} {
		b, err := os.ReadFile(name)
		if errors.Is(err, fs.ErrNotExist) {
			t.Skip("source files not available (test binary run outside the package directory)")
		}
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		if strings.Contains(src, "SURROGATE") {
			t.Errorf("%s carries a surrogate marker: it is not real generator output", name)
		}
		if name == "affine_amd64.s" && !strings.Contains(src, "VGF2P8AFFINEQB") {
			t.Errorf("%s does not use VGF2P8AFFINEQB", name)
		}
	}
}

// TestSurrogateMarkerKnownToGenerator keeps the marker used above in step
// with the generator.
func TestSurrogateMarkerKnownToGenerator(t *testing.T) {
	b, err := os.ReadFile("asmgen/x86.go")
	if errors.Is(err, fs.ErrNotExist) {
		t.Skip("source files not available (test binary run outside the package directory)")
	}
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `surrogateMarker = "`+surrogateMarker+`"`) {
		t.Fatal("asmgen/x86.go surrogateMarker differs from the marker the committed-file test checks")
	}
}
