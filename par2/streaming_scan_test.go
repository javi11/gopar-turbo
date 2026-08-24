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
