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

func TestMemFSOpenWritePositional(t *testing.T) {
	dir := memfs.RootDir()
	fs := memfs.MakeMemFS(dir, map[string][]byte{})
	path := filepath.Join(dir, "out.bin")

	w, closeFn, err := fs.OpenWrite(path, 8)
	require.NoError(t, err)
	_, err = w.WriteAt([]byte{5, 6}, 4) // out of order on purpose
	require.NoError(t, err)
	_, err = w.WriteAt([]byte{1, 2}, 0)
	require.NoError(t, err)
	require.NoError(t, closeFn())

	got, err := fs.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, []byte{1, 2, 0, 0, 5, 6, 0, 0}, got)
}

func TestMemFSOpenWriteTruncatesExisting(t *testing.T) {
	dir := memfs.RootDir()
	fs := memfs.MakeMemFS(dir, map[string][]byte{"out.bin": {9, 9, 9, 9, 9, 9}})
	path := filepath.Join(dir, "out.bin")

	w, closeFn, err := fs.OpenWrite(path, 3)
	require.NoError(t, err)
	_, err = w.WriteAt([]byte{1}, 0)
	require.NoError(t, err)
	require.NoError(t, closeFn())

	got, err := fs.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, []byte{1, 0, 0}, got)
}

func TestDefaultFileIOOpenWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.bin")

	w, closeFn, err := defaultFileIO{}.OpenWrite(path, 4)
	require.NoError(t, err)
	_, err = w.WriteAt([]byte{7, 8}, 2)
	require.NoError(t, err)
	require.NoError(t, closeFn())

	got, err := defaultFileIO{}.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, []byte{0, 0, 7, 8}, got)
}
