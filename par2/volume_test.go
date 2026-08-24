package par2

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/javi11/gopar-turbo/memfs"
	"github.com/stretchr/testify/require"
)

// walkPackets must see exactly the packets readNextPacket sees, in order.
func TestWalkPacketsMatchesReadFile(t *testing.T) {
	workingDir := memfs.RootDir()
	fs := makeShiftTestMemFS(workingDir)
	buildPAR2Data(t, fs, workingDir, 4, 2)

	volPath := filepath.Join(workingDir, "file.vol00+01.par2")
	data, err := fs.ReadFile(volPath)
	require.NoError(t, err)

	var walked []packetType
	r, size, closeFn, err := fs.OpenRead(volPath)
	require.NoError(t, err)
	defer closeFn()
	err = walkPackets(r, size, func(setID recoverySetID, typ packetType, body []byte) error {
		walked = append(walked, typ)
		return nil
	})
	require.NoError(t, err)
	require.NotEmpty(t, walked)

	// Reference: enumerate packets via the whole-buffer reader.
	buf := bytes.NewBuffer(data)
	var direct []packetType
	for {
		_, typ, _, rerr := readNextPacket(buf)
		if rerr != nil {
			break
		}
		direct = append(direct, typ)
	}
	require.Equal(t, direct, walked)
}

// A volume with a corrupted packet body must be rejected just like today.
func TestWalkPacketsRejectsCorruptPacket(t *testing.T) {
	workingDir := memfs.RootDir()
	fs := makeShiftTestMemFS(workingDir)
	buildPAR2Data(t, fs, workingDir, 4, 2)

	volPath := filepath.Join(workingDir, "file.vol00+01.par2")
	data, err := fs.ReadFile(volPath)
	require.NoError(t, err)
	data[len(data)-3] ^= 0xFF // clobber inside the last packet's body
	require.NoError(t, fs.WriteFile(volPath, data))

	r, size, closeFn, err := fs.OpenRead(volPath)
	require.NoError(t, err)
	defer closeFn()
	err = walkPackets(r, size, func(recoverySetID, packetType, []byte) error { return nil })
	require.Error(t, err, "hash-mismatched packet must surface an error")
}

// Streaming parity load must not read volume files whole, in either mode.
func TestLoadParityDoesNotReadVolumesWhole(t *testing.T) {
	workingDir := memfs.RootDir()

	for _, mode := range []string{"presence", "retain"} {
		t.Run(mode, func(t *testing.T) {
			fs := makeShiftTestMemFS(workingDir)
			buildPAR2Data(t, fs, workingDir, 4, 2)

			counting := newCountingFileIO(testFileIO{t, fs})
			d, err := newDecoder(counting, testDecoderDelegate{t},
				filepath.Join(workingDir, "file.par2"), 1, defaultScanPolicy(), 0)
			require.NoError(t, err)
			require.NoError(t, d.LoadFileData())

			if mode == "presence" {
				require.NoError(t, d.LoadParityPresence())
				for i, shard := range d.parityShards {
					require.Empty(t, shard, "presence mode must not retain shard %d", i)
				}
			} else {
				require.NoError(t, d.LoadParityData())
				for i, shard := range d.parityShards {
					require.Len(t, shard, 4, "retain mode must keep shard %d", i)
				}
			}
			require.Equal(t, 2, d.ShardCounts().UsableParityShardCount)
			require.Zero(t, counting.wholeReads["file.vol00+01.par2"],
				"volumes must be streamed, not read whole")
			require.Zero(t, counting.wholeReads["file.vol01+01.par2"])
		})
	}
}

// A volume from a different recovery set must be skipped, not fatal.
func TestLoadParitySkipsForeignSetVolume(t *testing.T) {
	workingDir := memfs.RootDir()
	fs := makeShiftTestMemFS(workingDir)
	buildPAR2Data(t, fs, workingDir, 4, 2)

	// Overwrite one volume with a whole different set's index file: every
	// packet carries a foreign set ID.
	other := memfs.MakeMemFS(workingDir, map[string][]byte{"zz.bin": {9, 9, 9, 9, 9}})
	buildPAR2Data(t, other, workingDir, 4, 1)
	foreign, err := other.ReadFile(filepath.Join(workingDir, "file.par2"))
	require.NoError(t, err)
	require.NoError(t, fs.WriteFile(filepath.Join(workingDir, "file.vol01+01.par2"), foreign))

	d, err := newDecoderForTest(t, fs, filepath.Join(workingDir, "file.par2"))
	require.NoError(t, err)
	require.NoError(t, d.LoadFileData())
	require.NoError(t, d.LoadParityData())
	require.Equal(t, 1, d.ShardCounts().UsableParityShardCount,
		"only the volume from our set should contribute")
}
