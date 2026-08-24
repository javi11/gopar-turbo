package par2

import (
	"path/filepath"
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
