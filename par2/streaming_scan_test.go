package par2

import (
	"path/filepath"
	"testing"

	"github.com/javi11/gopar-turbo/memfs"
	"github.com/stretchr/testify/require"
)

// The scan must report shard presence and where the bytes live, without
// retaining the payload.
func TestScanRecordsPresenceAndLocation(t *testing.T) {
	workingDir := memfs.RootDir()
	fs := makeShiftTestMemFS(workingDir)
	buildPAR2Data(t, fs, workingDir, 4, 2)

	d, err := newDecoderForTest(t, fs, filepath.Join(workingDir, "file.par2"))
	require.NoError(t, err)
	require.NoError(t, d.LoadFileData())

	info := d.fileIntegrityInfos[0]
	require.Len(t, info.shardInfos, 10)
	for i, si := range info.shardInfos {
		require.True(t, si.present, "shard %d should be present", i)
		require.Equal(t, i*4, si.foundAt.start, "shard %d location", i)
	}
}

func TestScanMarksCorruptShardAbsent(t *testing.T) {
	workingDir := memfs.RootDir()
	fs := makeShiftTestMemFS(workingDir)
	buildPAR2Data(t, fs, workingDir, 4, 2)
	perturbFile(t, fs, "big.rar")

	d, err := newDecoderForTest(t, fs, filepath.Join(workingDir, "file.par2"))
	require.NoError(t, err)
	require.NoError(t, d.LoadFileData())

	absent := 0
	for _, si := range d.fileIntegrityInfos[0].shardInfos {
		if !si.present {
			absent++
		}
	}
	require.Equal(t, 1, absent, "exactly the perturbed shard should be absent")
}

// readShard must return the shard's bytes from wherever the scan found them,
// zero-padding a trailing partial slice.
func TestReadShardReturnsBytes(t *testing.T) {
	workingDir := memfs.RootDir()
	fs := makeShiftTestMemFS(workingDir)
	buildPAR2Data(t, fs, workingDir, 4, 2)

	d, err := newDecoderForTest(t, fs, filepath.Join(workingDir, "file.par2"))
	require.NoError(t, err)
	require.NoError(t, d.LoadFileData())

	want, err := fs.ReadFile(filepath.Join(workingDir, "big.rar"))
	require.NoError(t, err)

	buf := make([]byte, 4)
	for i, si := range d.fileIntegrityInfos[0].shardInfos {
		require.NoError(t, d.readShard(si, buf))
		require.Equal(t, want[i*4:i*4+4], buf, "shard %d bytes", i)
	}
}

// countingFileIO records which paths were read whole, so a test can assert
// that the scan streams data files instead of materialising them.
type countingFileIO struct {
	fileIO
	wholeReads map[string]int
}

func newCountingFileIO(inner fileIO) *countingFileIO {
	return &countingFileIO{fileIO: inner, wholeReads: map[string]int{}}
}

func (c *countingFileIO) ReadFile(path string) ([]byte, error) {
	c.wholeReads[filepath.Base(path)]++
	return c.fileIO.ReadFile(path)
}

// The scan must not read data files whole. Index and volume files are small
// and are still allowed to be read whole.
func TestScanDoesNotReadDataFilesWhole(t *testing.T) {
	workingDir := memfs.RootDir()
	fs := makeShiftTestMemFS(workingDir)
	buildPAR2Data(t, fs, workingDir, 4, 2)

	counting := newCountingFileIO(testFileIO{t, fs})
	d, err := newDecoder(counting, testDecoderDelegate{t},
		filepath.Join(workingDir, "file.par2"), 1, defaultScanPolicy())
	require.NoError(t, err)
	require.NoError(t, d.LoadFileData())

	require.Zero(t, counting.wholeReads["big.rar"],
		"data files must be streamed, not read whole")
}

// A file much larger than the scan window must still scan correctly,
// including its trailing partial slice.
func TestScanHandlesFileLargerThanWindow(t *testing.T) {
	workingDir := memfs.RootDir()
	sliceByteCount := 16
	data := make([]byte, sliceByteCount*40+7) // 41 slices, last one partial
	for i := range data {
		data[i] = byte(i * 31)
	}
	fs := memfs.MakeMemFS(workingDir, map[string][]byte{"wide.rar": data})
	buildPAR2Data(t, fs, workingDir, sliceByteCount, 2)

	d, err := newDecoderForTest(t, fs, filepath.Join(workingDir, "file.par2"))
	require.NoError(t, err)
	require.NoError(t, d.LoadFileData())

	require.Len(t, d.fileIntegrityInfos[0].shardInfos, 41)
	for i, si := range d.fileIntegrityInfos[0].shardInfos {
		require.True(t, si.present, "shard %d", i)
	}
	require.False(t, d.ShardCounts().RepairNeeded())
}

// Verification needs to know how many parity shards exist, never their bytes.
// Holding them made verify memory scale with the recovery set.
func TestLoadParityPresenceRetainsNoBytes(t *testing.T) {
	workingDir := memfs.RootDir()
	fs := makeShiftTestMemFS(workingDir)
	buildPAR2Data(t, fs, workingDir, 4, 2)

	d, err := newDecoderForTest(t, fs, filepath.Join(workingDir, "file.par2"))
	require.NoError(t, err)
	require.NoError(t, d.LoadFileData())
	require.NoError(t, d.LoadParityPresence())

	counts := d.ShardCounts()
	require.Equal(t, 2, counts.UsableParityShardCount)
	require.Zero(t, counts.UnusableParityShardCount)

	for i, shard := range d.parityShards {
		require.Empty(t, shard, "parity shard %d must not be retained", i)
	}
}

// LoadParityData still yields usable bytes for repair.
func TestLoadParityDataRetainsBytes(t *testing.T) {
	workingDir := memfs.RootDir()
	fs := makeShiftTestMemFS(workingDir)
	buildPAR2Data(t, fs, workingDir, 4, 2)

	d, err := newDecoderForTest(t, fs, filepath.Join(workingDir, "file.par2"))
	require.NoError(t, err)
	require.NoError(t, d.LoadFileData())
	require.NoError(t, d.LoadParityData())

	require.Equal(t, 2, d.ShardCounts().UsableParityShardCount)
	for i, shard := range d.parityShards {
		require.Len(t, shard, 4, "parity shard %d", i)
	}
}
