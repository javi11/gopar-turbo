package par2

import (
	"path/filepath"
	"testing"

	"github.com/javi11/gopar-turbo/memfs"
	"github.com/stretchr/testify/require"
)

func TestMemFSOpenReadReturnsSizeAndBytes(t *testing.T) {
	dir := memfs.RootDir()
	fs := memfs.MakeMemFS(dir, map[string][]byte{
		"a.bin": {0x1, 0x2, 0x3, 0x4, 0x5},
	})

	r, size, closeFn, err := fs.OpenRead(filepath.Join(dir, "a.bin"))
	require.NoError(t, err)
	defer closeFn()
	require.Equal(t, int64(5), size)

	buf := make([]byte, 3)
	n, err := r.ReadAt(buf, 1)
	require.NoError(t, err)
	require.Equal(t, 3, n)
	require.Equal(t, []byte{0x2, 0x3, 0x4}, buf)
}

func TestMemFSOpenReadMissingFile(t *testing.T) {
	dir := memfs.RootDir()
	fs := memfs.MakeMemFS(dir, map[string][]byte{})
	_, _, _, err := fs.OpenRead(filepath.Join(dir, "nope.bin"))
	require.Error(t, err)
}

// defaultFileIO must report the same size and bytes for a real file.
func TestDefaultFileIOOpenRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.bin")
	require.NoError(t, defaultFileIO{}.WriteFile(path, []byte{9, 8, 7, 6}))

	r, size, closeFn, err := defaultFileIO{}.OpenRead(path)
	require.NoError(t, err)
	defer closeFn()
	require.Equal(t, int64(4), size)

	buf := make([]byte, 2)
	_, err = r.ReadAt(buf, 2)
	require.NoError(t, err)
	require.Equal(t, []byte{7, 6}, buf)
}
