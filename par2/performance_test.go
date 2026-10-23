package par2

import (
	"io"
	"math/rand"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/javi11/gopar-turbo/gf16"
	"github.com/javi11/gopar-turbo/memfs"
	"github.com/stretchr/testify/require"
)

type dataReadCounter struct {
	fileIO
	bytes atomic.Int64
}
type countedDataReader struct {
	io.ReaderAt
	bytes *atomic.Int64
}

func (r countedDataReader) ReadAt(p []byte, off int64) (int, error) {
	n, err := r.ReaderAt.ReadAt(p, off)
	r.bytes.Add(int64(n))
	return n, err
}
func (c *dataReadCounter) OpenRead(path string) (io.ReaderAt, int64, func() error, error) {
	r, n, closeFn, err := c.fileIO.OpenRead(path)
	if err == nil && filepath.Ext(path) == ".rar" {
		r = countedDataReader{r, &c.bytes}
	}
	return r, n, closeFn, err
}

func TestParityCheckReadsEachDataByteOnce(t *testing.T) {
	const slice = 1024
	for _, sparse := range []bool{false, true} {
		t.Run(map[bool]string{false: "dense", true: "sparse"}[sparse], func(t *testing.T) {
			data := make([]byte, 8*slice+37)
			rand.New(rand.NewSource(1)).Read(data)
			dir := memfs.RootDir()
			fs := memfs.MakeMemFS(dir, map[string][]byte{"big.rar": data})
			buildPAR2Data(t, fs, dir, slice, 32)
			c := &dataReadCounter{fileIO: testFileIO{t, fs}}
			d, err := newDecoder(c, DoNothingDecoderDelegate{}, filepath.Join(dir, "file.par2"), 4, defaultScanPolicy(), 128)
			require.NoError(t, err)
			require.NoError(t, d.LoadFileData())
			require.NoError(t, d.LoadParityData())
			if sparse {
				for i := 0; i < 31; i++ {
					d.parityPresent[i] = false
					d.parityLocations[i] = parityLocation{}
				}
			}
			c.bytes.Store(0)
			cache := newReaderCache(d)
			defer cache.Close()
			require.NoError(t, d.verifyParity(cache))
			require.Equal(t, int64(len(data)), c.bytes.Load(), "chunk passes must read disjoint ranges")
			// Actual stored parity must still be compared, including sparse rows.
			loc := d.parityLocations[31]
			volume, err := fs.ReadFile(loc.path)
			require.NoError(t, err)
			volume[loc.offset+int64(slice)-1] ^= 1
			require.NoError(t, fs.WriteFile(loc.path, volume))
			cache.Close()
			cache = newReaderCache(d)
			defer cache.Close()
			require.Error(t, d.verifyParity(cache))
		})
	}
}

func TestSparseRecoveryPlanAndRepair(t *testing.T) {
	dir := memfs.RootDir()
	fs := makeShiftTestMemFS(dir)
	buildPAR2Data(t, fs, dir, 4, 32)
	want := pristineBig(t, dir)
	perturbFile(t, fs, "big.rar")
	d, err := newDecoderForTest(t, fs, filepath.Join(dir, "file.par2"))
	require.NoError(t, err)
	require.NoError(t, d.LoadFileData())
	require.NoError(t, d.LoadParityData())
	for i := 0; i < 31; i++ {
		d.parityPresent[i] = false
		d.parityLocations[i] = parityLocation{}
	}
	available, missing, exponents, inputs, err := d.classifyReconstruction()
	require.NoError(t, err)
	require.Equal(t, []int{31}, exponents, "retain the actual on-disk exponent")
	require.Len(t, missing, 1)
	require.Len(t, inputs, len(available)+1, "include only selected recovery inputs")
	_, err = d.Repair(true)
	require.NoError(t, err)
	got, err := fs.ReadFile(filepath.Join(dir, "big.rar"))
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func TestChunkBudgetIncludesFoldBuffers(t *testing.T) {
	for _, missing := range []int{1, 10, 210} {
		const budget = 64 << 20
		chunk := chunkSizeFor(budget, 64<<20, missing)
		buffers := missing + 2
		if gf16.Accelerated() {
			buffers = 2*missing + 49
		}
		require.LessOrEqual(t, int64(chunk)*int64(buffers), int64(budget))
	}
}

func TestChunkBudgetHonorsLargerExplicitBudget(t *testing.T) {
	require.Greater(t, chunkSizeFor(1<<30, 8<<20, 210), chunkSizeFor(0, 8<<20, 210))
}

func TestModerateChunkBudgetAvoidsTwoBytePasses(t *testing.T) {
	require.GreaterOrEqual(t, chunkSizeFor(1<<20, 8<<20, 210), 1024)
}

func TestRepairMaximumRecoveryExponent(t *testing.T) {
	dir := memfs.RootDir()
	fs := makeShiftTestMemFS(dir)
	buildPAR2Data(t, fs, dir, 4, 1)
	want := pristineBig(t, dir)
	paths, err := fs.FindWithPrefixAndSuffix(filepath.Join(dir, "file.vol"), ".par2")
	require.NoError(t, err)
	require.Len(t, paths, 1)
	volume, err := fs.ReadFile(paths[0])
	require.NoError(t, err)
	_, f, err := readFile(DoNothingDecoderDelegate{}, nil, volume)
	require.NoError(t, err)
	// Generator^65535 == Generator^0 == 1, so this block has identical data.
	f.recoveryPackets[65535] = f.recoveryPackets[0]
	delete(f.recoveryPackets, 0)
	_, volume, err = writeFile(f)
	require.NoError(t, err)
	require.NoError(t, fs.WriteFile(paths[0], volume))
	perturbFile(t, fs, "big.rar")
	_, err = repair(testFileIO{t, fs}, filepath.Join(dir, "file.par2"), RepairOptions{DoubleCheck: true})
	require.NoError(t, err)
	got, err := fs.ReadFile(filepath.Join(dir, "big.rar"))
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func TestChunkedRepairHeavyDamage(t *testing.T) {
	const slice = 1024
	data := make([]byte, 256*slice+37)
	rand.New(rand.NewSource(71)).Read(data)
	for _, tc := range []struct {
		name   string
		budget int
	}{{"default", 0}, {"chunked", 64 << 10}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := memfs.RootDir()
			fs := memfs.MakeMemFS(dir, map[string][]byte{"big.rar": data})
			buildPAR2Data(t, fs, dir, slice, 64)
			damaged := append([]byte(nil), data...)
			for i := 0; i < 31; i++ {
				damaged[2*i*slice] ^= 0xFF
			}
			damaged[len(data)-1] ^= 0xFF
			require.NoError(t, fs.WriteFile(filepath.Join(dir, "big.rar"), damaged))
			_, err := repair(testFileIO{t, fs}, filepath.Join(dir, "file.par2"), RepairOptions{DoubleCheck: true, MemoryBudget: tc.budget, NumGoroutines: 4})
			require.NoError(t, err)
			got, err := fs.ReadFile(filepath.Join(dir, "big.rar"))
			require.NoError(t, err)
			require.Equal(t, data, got)
		})
	}
}

func TestDenseWrittenVerificationUsesWindows(t *testing.T) {
	const slice = 1024
	data := make([]byte, 128*slice+37)
	rand.New(rand.NewSource(91)).Read(data)
	dir := memfs.RootDir()
	fs := memfs.MakeMemFS(dir, map[string][]byte{"big.rar": data})
	buildPAR2Data(t, fs, dir, slice, 4)
	counter := &dataReadCounter{fileIO: testFileIO{t, fs}}
	d, err := newDecoder(counter, DoNothingDecoderDelegate{}, filepath.Join(dir, "file.par2"), 4, defaultScanPolicy(), 0)
	require.NoError(t, err)
	paths := make([]string, len(d.recoverySet))
	rows := make([][]int, len(paths))
	target := -1
	for i, info := range d.recoverySet {
		if filepath.Base(d.getFilePath(info)) == "big.rar" {
			target = i
			paths[i] = d.getFilePath(info)
			for j := 0; j < 96; j++ {
				rows[i] = append(rows[i], j)
			}
		}
	}
	require.NotEqual(t, -1, target)
	counter.bytes.Store(0)
	errs := d.checkWrittenFiles(paths, rows)
	require.NoError(t, errs[target])
	require.Equal(t, int64(len(data)), counter.bytes.Load(), "dense checks should read one sequential pass")
	rows[target] = []int{80}
	counter.bytes.Store(0)
	require.NoError(t, d.checkWrittenFiles(paths, rows)[target])
	require.Equal(t, int64(16*1024+slice), counter.bytes.Load(), "sparse checks must handle a 16 KiB head larger than one slice")
	damaged := append([]byte(nil), data...)
	damaged[80*slice+17] ^= 1
	require.NoError(t, fs.WriteFile(paths[target], damaged))
	require.Error(t, d.checkWrittenFiles(paths, rows)[target])
}
