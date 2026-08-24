package par2

import (
	"errors"
	"io"
	"path/filepath"
	"sync"
	"testing"

	"github.com/javi11/gopar-turbo/memfs"
	"github.com/stretchr/testify/require"
)

func pristineBig(t *testing.T, workingDir string) []byte {
	t.Helper()
	want := makeShiftTestMemFS(workingDir)
	data, err := want.ReadFile(filepath.Join(workingDir, "big.rar"))
	require.NoError(t, err)
	return data
}

// Repair must reconstruct byte-identical content while streaming.
func TestStreamingRepairRestoresExactBytes(t *testing.T) {
	workingDir := memfs.RootDir()
	fs := makeShiftTestMemFS(workingDir)
	buildPAR2Data(t, fs, workingDir, 4, 2)
	want := pristineBig(t, workingDir)

	perturbFile(t, fs, "big.rar")

	result, err := repair(testFileIO{t, fs}, filepath.Join(workingDir, "file.par2"), RepairOptions{})
	require.NoError(t, err)
	require.Len(t, result.RepairedPaths, 1)

	got, err := fs.ReadFile(filepath.Join(workingDir, "big.rar"))
	require.NoError(t, err)
	require.Equal(t, want, got)
}

// A budget small enough to force several chunk passes must not change output.
func TestStreamingRepairChunkedMatchesUnchunked(t *testing.T) {
	workingDir := memfs.RootDir()

	run := func(budget int) []byte {
		t.Helper()
		fs := makeShiftTestMemFS(workingDir)
		buildPAR2Data(t, fs, workingDir, 4, 2)
		perturbFile(t, fs, "big.rar")
		_, err := repair(testFileIO{t, fs}, filepath.Join(workingDir, "file.par2"),
			RepairOptions{MemoryBudget: budget})
		require.NoError(t, err)
		data, err := fs.ReadFile(filepath.Join(workingDir, "big.rar"))
		require.NoError(t, err)
		return data
	}

	want := pristineBig(t, workingDir)
	require.Equal(t, want, run(0), "default budget")
	require.Equal(t, want, run(2), "a tiny budget forces several chunk passes")
	require.Equal(t, want, run(1<<30), "a huge budget must not change the result")
}

// Repair must not read data files whole, even while reconstructing.
func TestStreamingRepairDoesNotReadDataFilesWhole(t *testing.T) {
	workingDir := memfs.RootDir()
	fs := makeShiftTestMemFS(workingDir)
	buildPAR2Data(t, fs, workingDir, 4, 2)
	perturbFile(t, fs, "big.rar")

	counting := newCountingFileIO(testFileIO{t, fs})
	_, err := repair(counting, filepath.Join(workingDir, "file.par2"), RepairOptions{})
	require.NoError(t, err)

	require.Zero(t, counting.wholeReads["big.rar"],
		"repair must stream data files, not read them whole")
}

// Deleting a whole file still repairs, which exercises the path where every
// shard of a file is missing rather than just one.
func TestStreamingRepairRestoresDeletedFile(t *testing.T) {
	workingDir := memfs.RootDir()
	fs := makeShiftTestMemFS(workingDir)
	// big.rar is 10 shards, so recovering the whole file needs 10 recovery
	// blocks. With fewer, "not enough parity shards" is the right answer.
	buildPAR2Data(t, fs, workingDir, 4, 10)
	want := pristineBig(t, workingDir)

	_, err := fs.RemoveFile(filepath.Join(workingDir, "big.rar"))
	require.NoError(t, err)

	result, err := repair(testFileIO{t, fs}, filepath.Join(workingDir, "file.par2"), RepairOptions{})
	require.NoError(t, err)
	require.Len(t, result.RepairedPaths, 1)

	got, err := fs.ReadFile(filepath.Join(workingDir, "big.rar"))
	require.NoError(t, err)
	require.Equal(t, want, got)
}

// Too few recovery blocks to rebuild a deleted file must fail cleanly rather
// than produce wrong bytes.
func TestStreamingRepairDeletedFileWithoutEnoughParity(t *testing.T) {
	workingDir := memfs.RootDir()
	fs := makeShiftTestMemFS(workingDir)
	buildPAR2Data(t, fs, workingDir, 4, 2)

	_, err := fs.RemoveFile(filepath.Join(workingDir, "big.rar"))
	require.NoError(t, err)

	_, err = repair(testFileIO{t, fs}, filepath.Join(workingDir, "file.par2"), RepairOptions{})
	require.True(t, RepairErrorMeansRepairNecessaryButNotPossible(err),
		"expected a repair-not-possible error, got %v", err)
}

// The default accumulator footprint is capped: on a set where one accumulator
// per missing shard would exceed the cap, chunkSizeFor must split slices even
// with no explicit budget.
func TestChunkSizeForCapsDefaultFootprint(t *testing.T) {
	sliceByteCount := 2380956
	missing := 210 // the benchmark set's repair-missing shape: ~504 MB uncapped

	chunk := chunkSizeFor(0, sliceByteCount, missing)
	require.Less(t, chunk, sliceByteCount,
		"default must chunk when uncapped accumulators exceed the cap")
	require.LessOrEqual(t, chunk*missing, foldAccumulatorCap)
	require.Zero(t, chunk%2, "gf16 requires even sizes")

	// An explicit budget below the cap still wins.
	tight := chunkSizeFor(1<<20, sliceByteCount, missing)
	require.LessOrEqual(t, tight*missing, 1<<20)

	// Small sets stay single-pass.
	require.Equal(t, 4096, chunkSizeFor(0, 4096, 10))
}

// writeRecordingFileIO distinguishes whole-file writes from positional ones.
type writeRecordingFileIO struct {
	fileIO
	mu         sync.Mutex
	wholeFiles []string
	positional []string
}

func (w *writeRecordingFileIO) WriteFile(path string, data []byte) error {
	w.mu.Lock()
	w.wholeFiles = append(w.wholeFiles, filepath.Base(path))
	w.mu.Unlock()
	return w.fileIO.WriteFile(path, data)
}

func (w *writeRecordingFileIO) OpenWrite(path string, size int64) (io.WriterAt, func(commit bool) error, error) {
	w.mu.Lock()
	w.positional = append(w.positional, filepath.Base(path))
	w.mu.Unlock()
	return w.fileIO.OpenWrite(path, size)
}

// Repair must not buffer whole reconstructed files.
func TestRepairWritesIncrementally(t *testing.T) {
	workingDir := memfs.RootDir()
	fs := makeShiftTestMemFS(workingDir)
	buildPAR2Data(t, fs, workingDir, 4, 2)
	want := pristineBig(t, workingDir)
	perturbFile(t, fs, "big.rar")

	rec := &writeRecordingFileIO{fileIO: testFileIO{t, fs}}
	_, err := repair(rec, filepath.Join(workingDir, "file.par2"), RepairOptions{})
	require.NoError(t, err)

	require.Contains(t, rec.positional, "big.rar")
	require.NotContains(t, rec.wholeFiles, "big.rar",
		"repaired data files must be written positionally, not buffered whole")

	got, err := fs.ReadFile(filepath.Join(workingDir, "big.rar"))
	require.NoError(t, err)
	require.Equal(t, want, got)
}

// Many chunk passes through the incremental writer must not change output,
// including the trailing partial slice.
func TestIncrementalRepairChunkedByteIdentical(t *testing.T) {
	workingDir := memfs.RootDir()
	fs := makeShiftTestMemFS(workingDir) // 40 bytes: 10 slices of 4
	buildPAR2Data(t, fs, workingDir, 4, 10)
	want := pristineBig(t, workingDir)
	_, err := fs.RemoveFile(filepath.Join(workingDir, "big.rar"))
	require.NoError(t, err)

	_, err = repair(testFileIO{t, fs}, filepath.Join(workingDir, "file.par2"),
		RepairOptions{MemoryBudget: 2}) // chunk = 2 bytes -> 2 passes per slice
	require.NoError(t, err)

	got, err := fs.ReadFile(filepath.Join(workingDir, "big.rar"))
	require.NoError(t, err)
	require.Equal(t, want, got)
}

// windowsLikeFileIO reproduces Windows rename semantics: committing a write to
// a path fails while any reader of that path is still open. POSIX allows the
// rename, so without this double the hazard is invisible on the dev machine
// and only shows up in Windows CI.
type windowsLikeFileIO struct {
	fileIO
	mu   sync.Mutex
	open map[string]int
}

func newWindowsLikeFileIO(inner fileIO) *windowsLikeFileIO {
	return &windowsLikeFileIO{fileIO: inner, open: map[string]int{}}
}

func (w *windowsLikeFileIO) OpenRead(path string) (io.ReaderAt, int64, func() error, error) {
	r, size, closeFn, err := w.fileIO.OpenRead(path)
	if err != nil {
		return r, size, closeFn, err
	}
	w.mu.Lock()
	w.open[path]++
	w.mu.Unlock()

	var once sync.Once
	return r, size, func() error {
		once.Do(func() {
			w.mu.Lock()
			w.open[path]--
			w.mu.Unlock()
		})
		return closeFn()
	}, nil
}

func (w *windowsLikeFileIO) OpenWrite(path string, size int64) (io.WriterAt, func(bool) error, error) {
	writer, closeFn, err := w.fileIO.OpenWrite(path, size)
	if err != nil {
		return writer, closeFn, err
	}
	return writer, func(commit bool) error {
		if commit {
			w.mu.Lock()
			n := w.open[path]
			w.mu.Unlock()
			if n > 0 {
				return errors.New("access is denied: file still open")
			}
		}
		return closeFn(commit)
	}, nil
}

// Repair must not hold a reader on a file while committing its replacement.
func TestRepairCommitsWithNoReadersOpen(t *testing.T) {
	workingDir := memfs.RootDir()

	t.Run("corrupted file", func(t *testing.T) {
		fs := makeShiftTestMemFS(workingDir)
		buildPAR2Data(t, fs, workingDir, 4, 2)
		want := pristineBig(t, workingDir)
		perturbFile(t, fs, "big.rar")

		_, err := repair(newWindowsLikeFileIO(testFileIO{t, fs}),
			filepath.Join(workingDir, "file.par2"), RepairOptions{})
		require.NoError(t, err)

		got, err := fs.ReadFile(filepath.Join(workingDir, "big.rar"))
		require.NoError(t, err)
		require.Equal(t, want, got)
	})

	t.Run("missing file", func(t *testing.T) {
		fs := makeShiftTestMemFS(workingDir)
		buildPAR2Data(t, fs, workingDir, 4, 10)
		want := pristineBig(t, workingDir)
		_, err := fs.RemoveFile(filepath.Join(workingDir, "big.rar"))
		require.NoError(t, err)

		_, err = repair(newWindowsLikeFileIO(testFileIO{t, fs}),
			filepath.Join(workingDir, "file.par2"), RepairOptions{})
		require.NoError(t, err)

		got, err := fs.ReadFile(filepath.Join(workingDir, "big.rar"))
		require.NoError(t, err)
		require.Equal(t, want, got)
	})
}
