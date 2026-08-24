package par2

import (
	"path/filepath"
	"testing"

	"github.com/javi11/gopar-turbo/memfs"
	"github.com/stretchr/testify/require"
)

// shiftFileRight rewrites path with padCount junk bytes prepended, so every
// shard still exists in the file but no longer starts on a slice boundary.
func shiftFileRight(t *testing.T, fs memfs.MemFS, path string, padCount int) {
	t.Helper()
	data, err := fs.ReadFile(path)
	require.NoError(t, err)
	shifted := make([]byte, padCount+len(data))
	for i := 0; i < padCount; i++ {
		shifted[i] = 0xAA
	}
	copy(shifted[padCount:], data)
	require.NoError(t, fs.WriteFile(path, shifted))
}

func TestFillShardInfosMissAdvancesWholeSliceWhenSearchDisabled(t *testing.T) {
	sliceByteCount := 4
	dataByteCount := 50
	id, _, checksumToLocation, fileIntegrityInfos, fileIDIndices, unrelatedData :=
		makeTestFillShardInfoInputs(t, sliceByteCount, dataByteCount)

	hits, misses := fillShardInfos(sliceByteCount, unrelatedData, checksumToLocation,
		id, fileIntegrityInfos, fileIDIndices, scanPolicy{findMisaligned: false})

	expectedSlices := (dataByteCount + sliceByteCount - 1) / sliceByteCount
	require.Equal(t, 0, hits)
	require.Equal(t, expectedSlices, misses,
		"with the search disabled a miss must advance a whole slice, "+
			"so misses counts slices, not bytes")
}

func TestFillShardInfosFindsShiftedDataOnlyWhenSearchEnabled(t *testing.T) {
	sliceByteCount := 4
	dataByteCount := 48
	id, data, checksumToLocation, fileIntegrityInfos, fileIDIndices, _ :=
		makeTestFillShardInfoInputs(t, sliceByteCount, dataByteCount)

	// Every shard is still present, just pushed off its slice boundary.
	shifted := append([]byte{0xAA, 0xAA}, data...)

	t.Run("enabled finds them", func(t *testing.T) {
		_, _, checksums, infos, indices, _ :=
			makeTestFillShardInfoInputs(t, sliceByteCount, dataByteCount)
		hits, _ := fillShardInfos(sliceByteCount, shifted, checksums,
			id, infos, indices, scanPolicy{findMisaligned: true})
		require.Equal(t, dataByteCount/sliceByteCount, hits)
	})

	t.Run("disabled finds none", func(t *testing.T) {
		hits, _ := fillShardInfos(sliceByteCount, shifted, checksumToLocation,
			id, fileIntegrityInfos, fileIDIndices, scanPolicy{findMisaligned: false})
		require.Equal(t, 0, hits)
	})
}

func TestFillShardInfosSearchLimitBoundsSlide(t *testing.T) {
	sliceByteCount := 16
	dataByteCount := 64
	id, data, _, _, _, _ := makeTestFillShardInfoInputs(t, sliceByteCount, dataByteCount)

	// Shift by more than the limit we are about to impose.
	shifted := append(make([]byte, 8), data...)
	for i := 0; i < 8; i++ {
		shifted[i] = 0xAA
	}

	t.Run("limit shorter than the shift finds nothing", func(t *testing.T) {
		_, _, checksums, infos, indices, _ :=
			makeTestFillShardInfoInputs(t, sliceByteCount, dataByteCount)
		hits, _ := fillShardInfos(sliceByteCount, shifted, checksums,
			id, infos, indices, scanPolicy{findMisaligned: true, searchLimit: 3})
		require.Equal(t, 0, hits)
	})

	t.Run("limit longer than the shift finds them", func(t *testing.T) {
		_, _, checksums, infos, indices, _ :=
			makeTestFillShardInfoInputs(t, sliceByteCount, dataByteCount)
		hits, _ := fillShardInfos(sliceByteCount, shifted, checksums,
			id, infos, indices, scanPolicy{findMisaligned: true, searchLimit: 32})
		require.Equal(t, dataByteCount/sliceByteCount, hits)
	})
}

// makeShiftTestMemFS builds a set with a single file big enough that shifting
// it strands more shards than there is parity to rebuild them.
func makeShiftTestMemFS(workingDir string) memfs.MemFS {
	data := make([]byte, 40)
	for i := range data {
		data[i] = byte(i * 7)
	}
	return memfs.MakeMemFS(workingDir, map[string][]byte{"big.rar": data})
}

func TestRepairAlignedCorruptionWithSearchDisabled(t *testing.T) {
	workingDir := memfs.RootDir()
	fs := makeShiftTestMemFS(workingDir)
	buildPAR2Data(t, fs, workingDir, 4, 2)
	parPath := filepath.Join(workingDir, "file.par2")
	bigPath := "big.rar"

	// Ordinary, slice-aligned corruption must still repair with the
	// expensive misaligned search switched off.
	perturbFile(t, fs, bigPath)

	result, err := repair(testFileIO{t, fs}, parPath, RepairOptions{})
	require.NoError(t, err)
	require.Equal(t, []string{filepath.Join(workingDir, bigPath)}, result.RepairedPaths)

	repaired, err := fs.ReadFile(filepath.Join(workingDir, bigPath))
	require.NoError(t, err)
	want := makeShiftTestMemFS(workingDir)
	wantData, err := want.ReadFile(filepath.Join(workingDir, bigPath))
	require.NoError(t, err)
	require.Equal(t, wantData, repaired)
}

func TestRepairMisalignedDataNeedsSearchEnabled(t *testing.T) {
	workingDir := memfs.RootDir()
	bigPath := "big.rar"

	// Shifting the file strands all 10 shards; only 2 parity shards exist,
	// so recovery is possible only by locating the shards where they sit.
	t.Run("enabled repairs", func(t *testing.T) {
		fs := makeShiftTestMemFS(workingDir)
		buildPAR2Data(t, fs, workingDir, 4, 2)
		shiftFileRight(t, fs, filepath.Join(workingDir, bigPath), 2)

		result, err := repair(testFileIO{t, fs}, filepath.Join(workingDir, "file.par2"),
			RepairOptions{FindMisalignedData: true})
		require.NoError(t, err)
		require.Equal(t, []string{filepath.Join(workingDir, bigPath)}, result.RepairedPaths)

		repaired, err := fs.ReadFile(filepath.Join(workingDir, bigPath))
		require.NoError(t, err)
		want := makeShiftTestMemFS(workingDir)
		wantData, err := want.ReadFile(filepath.Join(workingDir, bigPath))
		require.NoError(t, err)
		require.Equal(t, wantData, repaired)
	})

	t.Run("disabled cannot repair", func(t *testing.T) {
		fs := makeShiftTestMemFS(workingDir)
		buildPAR2Data(t, fs, workingDir, 4, 2)
		shiftFileRight(t, fs, filepath.Join(workingDir, bigPath), 2)

		_, err := repair(testFileIO{t, fs}, filepath.Join(workingDir, "file.par2"),
			RepairOptions{})
		require.True(t, RepairErrorMeansRepairNecessaryButNotPossible(err),
			"without the search the shifted shards are invisible, so repair "+
				"must fail rather than silently succeed; got %v", err)
	})
}
