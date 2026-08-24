package par2

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/javi11/gopar-turbo/memfs"
	"github.com/stretchr/testify/require"
)

// orderRecordingDelegate records OnDataFileLoad call order; other callbacks
// fall through to the do-nothing delegate. It must be safe for concurrent
// use of the other callbacks.
type orderRecordingDelegate struct {
	DoNothingDecoderDelegate
	mu    sync.Mutex
	loads []int
}

func (d *orderRecordingDelegate) OnDataFileLoad(i, n int, path string, byteCount, hits, misses int, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.loads = append(d.loads, i)
}

func makeManyFilesMemFS(workingDir string, numFiles, fileSize int) memfs.MemFS {
	files := make(map[string][]byte, numFiles)
	for f := 0; f < numFiles; f++ {
		data := make([]byte, fileSize)
		for i := range data {
			data[i] = byte(f*31 + i*7)
		}
		files[fmt.Sprintf("part%02d.bin", f)] = data
	}
	return memfs.MakeMemFS(workingDir, files)
}

// Parallel scan must produce identical results to goroutines=1, and emit
// OnDataFileLoad strictly in file order.
func TestParallelScanMatchesSequential(t *testing.T) {
	workingDir := memfs.RootDir()

	scan := func(goroutines int) (ShardCounts, []int) {
		fs := makeManyFilesMemFS(workingDir, 12, 40)
		buildPAR2Data(t, fs, workingDir, 4, 3)
		perturbFile(t, fs, "part03.bin")
		perturbFile(t, fs, "part07.bin")

		del := &orderRecordingDelegate{}
		d, err := newDecoder(testFileIO{t, fs}, del,
			filepath.Join(workingDir, "file.par2"), goroutines, defaultScanPolicy(), 0)
		require.NoError(t, err)
		require.NoError(t, d.LoadFileData())
		require.NoError(t, d.LoadParityPresence())
		return d.ShardCounts(), del.loads
	}

	seqCounts, seqOrder := scan(1)
	parCounts, parOrder := scan(8)
	require.Equal(t, seqCounts, parCounts)
	require.Equal(t, seqOrder, parOrder, "OnDataFileLoad must stay in file order")
	for i, v := range parOrder {
		require.Equal(t, i+1, v)
	}
}

// Repair after a parallel scan must produce the same bytes as after a
// sequential scan, including with duplicate slices across files.
func TestParallelScanRepairIdentical(t *testing.T) {
	workingDir := memfs.RootDir()

	run := func(goroutines int) map[string][]byte {
		fs := makeManyFilesMemFS(workingDir, 12, 40)
		// Duplicate content: two files share bytes, exercising cross-file
		// match recording under concurrency.
		dup, err := fs.ReadFile(filepath.Join(workingDir, "part01.bin"))
		require.NoError(t, err)
		require.NoError(t, fs.WriteFile(filepath.Join(workingDir, "part09.bin"), dup))
		buildPAR2Data(t, fs, workingDir, 4, 3)
		perturbFile(t, fs, "part05.bin")

		_, err = repair(testFileIO{t, fs}, filepath.Join(workingDir, "file.par2"),
			RepairOptions{NumGoroutines: goroutines})
		require.NoError(t, err)

		outs := map[string][]byte{}
		for f := 0; f < 12; f++ {
			name := fmt.Sprintf("part%02d.bin", f)
			data, err := fs.ReadFile(filepath.Join(workingDir, name))
			require.NoError(t, err)
			outs[name] = data
		}
		return outs
	}

	require.Equal(t, run(1), run(8))
}

// Verify and repair results must be unchanged with the scan and parity-load
// phases overlapped.
func TestOverlappedPhasesMatchSequential(t *testing.T) {
	workingDir := memfs.RootDir()
	fs := makeManyFilesMemFS(workingDir, 8, 40)
	buildPAR2Data(t, fs, workingDir, 4, 3)
	perturbFile(t, fs, "part02.bin")

	vr, err := verify(testFileIO{t, fs}, filepath.Join(workingDir, "file.par2"), VerifyOptions{})
	require.NoError(t, err)
	require.True(t, vr.ShardCounts.RepairNeeded())
	require.True(t, vr.ShardCounts.RepairPossible())
	require.Equal(t, 3, vr.ShardCounts.UsableParityShardCount)

	_, err = repair(testFileIO{t, fs}, filepath.Join(workingDir, "file.par2"), RepairOptions{})
	require.NoError(t, err)

	vr, err = verify(testFileIO{t, fs}, filepath.Join(workingDir, "file.par2"), VerifyOptions{})
	require.NoError(t, err)
	require.False(t, vr.ShardCounts.RepairNeeded())
}
