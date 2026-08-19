package par2

import (
	"crypto/md5"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// copyRealtoolFixture copies par2/testdata/realtool into a temp dir and
// returns that dir plus the expected md5 (hex) of each data file.
func copyRealtoolFixture(t *testing.T) (dir string, wantMD5 map[string]string) {
	t.Helper()
	dir = t.TempDir()
	entries, err := os.ReadDir("testdata/realtool")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join("testdata/realtool", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, e.Name()), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sums, err := os.ReadFile(filepath.Join(dir, "checksums.md5"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Fields(strings.TrimSpace(string(sums)))
	wantMD5 = map[string]string{"fileA.bin": lines[0], "fileB.bin": lines[1]}
	return dir, wantMD5
}

func fileMD5(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := md5.Sum(data)
	return hex.EncodeToString(sum[:])
}

func TestRealToolVerifyIntact(t *testing.T) {
	dir, _ := copyRealtoolFixture(t)
	result, err := Verify(filepath.Join(dir, "testset.par2"), VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.ShardCounts.RepairNeeded() {
		t.Error("intact fixture reported as needing repair")
	}
}

func TestRealToolRepairDeletedFile(t *testing.T) {
	dir, wantMD5 := copyRealtoolFixture(t)
	if err := os.Remove(filepath.Join(dir, "fileA.bin")); err != nil {
		t.Fatal(err)
	}
	result, err := Repair(filepath.Join(dir, "testset.par2"), RepairOptions{DoubleCheck: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.RepairedPaths) == 0 {
		t.Fatal("expected repaired paths")
	}
	for name, want := range wantMD5 {
		if got := fileMD5(t, filepath.Join(dir, name)); got != want {
			t.Errorf("%s: md5 = %s, want %s", name, got, want)
		}
	}
}

func TestRealToolRepairCorruptedFile(t *testing.T) {
	dir, wantMD5 := copyRealtoolFixture(t)
	path := filepath.Join(dir, "fileB.bin")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 100; i < 5000; i++ { // stomp across a slice boundary
		data[i] ^= 0xff
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Repair(filepath.Join(dir, "testset.par2"), RepairOptions{DoubleCheck: true}); err != nil {
		t.Fatal(err)
	}
	for name, want := range wantMD5 {
		if got := fileMD5(t, filepath.Join(dir, name)); got != want {
			t.Errorf("%s: md5 = %s, want %s", name, got, want)
		}
	}
}
