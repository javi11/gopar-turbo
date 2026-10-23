package par2

import (
	"errors"
	"io"
	"sync"

	"github.com/javi11/gopar-turbo/rsec16"
)

// parityLocation is where a recovery block's data lives on disk. Blocks are
// read back per chunk pass rather than retained: on a large set they are
// hundreds of megabytes in aggregate.
type parityLocation struct {
	path   string
	offset int64
	length int
	// vol is the index of the volume (in match order) the block was taken
	// from, so a parallel load still lets the first volume win duplicates.
	vol int
}

// readerCache keeps data files open for the duration of a reconstruction.
// Reconstruction reads every surviving shard once per chunk pass, so reopening
// a file per shard would dominate the cost.
//
// The cache is safe for concurrent use: reconstruction loads fold inputs from
// several goroutines. Opening is serialized by mu; ReadAt on an open reader
// is concurrency-safe by contract.
type readerCache struct {
	d       *Decoder
	mu      sync.Mutex
	readers map[string]io.ReaderAt
	sizes   map[string]int64
	closers []func() error
}

func newReaderCache(d *Decoder) *readerCache {
	return &readerCache{
		d:       d,
		readers: make(map[string]io.ReaderAt),
		sizes:   make(map[string]int64),
	}
}

// getPath opens path once and keeps it open for the cache's lifetime.
func (c *readerCache) getPath(path string) (io.ReaderAt, int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r, ok := c.readers[path]; ok {
		return r, c.sizes[path], nil
	}
	r, size, closeFn, err := c.d.fileIO.OpenRead(path)
	if err != nil {
		return nil, 0, err
	}
	c.readers[path] = r
	c.sizes[path] = size
	c.closers = append(c.closers, closeFn)
	return r, size, nil
}

func (c *readerCache) get(id fileID) (io.ReaderAt, int64, error) {
	idx, ok := c.d.fileIDIndices[id]
	if !ok {
		return nil, 0, errors.New("unknown file for shard")
	}
	return c.getPath(c.d.getFilePath(c.d.recoverySet[idx]))
}

// readAbs fills buf from path at an absolute offset, zero-padding past end.
func (c *readerCache) readAbs(path string, off int64, buf []byte) error {
	clear(buf)
	r, size, err := c.getPath(path)
	if err != nil {
		return err
	}
	if off >= size {
		return nil
	}
	n := int64(len(buf))
	if off+n > size {
		n = size - off
	}
	_, err = r.ReadAt(buf[:n], off)
	return err
}

func (c *readerCache) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, closeFn := range c.closers {
		_ = closeFn()
	}
	c.closers = nil
	c.readers = nil
}

// readRange fills buf with the shard's bytes starting off bytes into the
// shard, zero-padding anything past the end of the file. That padding matches
// sliceAndPadByteArray, which is how the shard's checksum was computed.
func (c *readerCache) readRange(info shardIntegrityInfo, off int, buf []byte) error {
	clear(buf)
	if !info.present {
		return errors.New("shard not present")
	}
	r, size, err := c.get(info.foundAt.fileID)
	if err != nil {
		return err
	}

	start := int64(info.foundAt.start + off)
	n := int64(len(buf))
	if start >= size {
		return nil
	}
	if start+n > size {
		n = size - start
	}
	_, err = r.ReadAt(buf[:n], start)
	return err
}

// foldInput identifies one input row of the reconstruction: either a
// surviving data shard, which is re-read from disk, or a recovery block.
type foldInput struct {
	isParity  bool
	parityIdx int
	shard     shardIntegrityInfo
}

// reconstructionPlan is everything needed to rebuild the missing shards,
// worked out from the scan without reading any shard data.
type reconstructionPlan struct {
	matrix      interface{ At(i, j int) uint16 }
	missingRows []int
	inputs      []foldInput
}

// defaultFoldMemoryBudget is the default bulk folding buffer budget. Explicit
// budgets may be larger to reduce strided disk I/O on large repairs.
const defaultFoldMemoryBudget = 512 << 20

// chunkSizeFor budgets caller outputs, prepared accumulators, two banks of
// up to 16 prepared inputs, up to 16 raw inputs and a zero buffer. Small
// buffers incur backend alignment overhead; matrices, metadata, scan buffers
// and native contexts are separate. A two-byte minimum permits tiny budgets.
func chunkSizeFor(budget, sliceByteCount, missing int) int {
	if missing <= 0 {
		return sliceByteCount
	}
	if budget <= 0 {
		budget = min(defaultMemoryBudget(), defaultFoldMemoryBudget)
	}
	return rsec16.FoldChunkSize(budget, sliceByteCount, missing)
}

// classifyReconstruction classifies every shard as available or missing and picks
// the recovery blocks that will stand in for the missing ones.
func (d *Decoder) classifyReconstruction() ([]int, []int, []int, []foldInput, error) {
	var availableRows, missingRows []int
	var inputs []foldInput

	totalShards := 0
	for _, info := range d.fileIntegrityInfos {
		totalShards += len(info.shardInfos)
	}
	if totalShards == 0 {
		return nil, nil, nil, nil, errors.New("no file integrity info")
	}
	if len(d.parityPresent) == 0 {
		return nil, nil, nil, nil, errors.New("no parity shards")
	}

	idx := 0
	for _, info := range d.fileIntegrityInfos {
		for _, si := range info.shardInfos {
			if si.present {
				availableRows = append(availableRows, idx)
				inputs = append(inputs, foldInput{shard: si})
			} else {
				missingRows = append(missingRows, idx)
			}
			idx++
		}
	}

	var parityExponents []int
	for i := 0; i < len(d.parityPresent) && len(inputs) < totalShards; i++ {
		if !d.parityPresent[i] {
			continue
		}
		parityExponents = append(parityExponents, i)
		inputs = append(inputs, foldInput{isParity: true, parityIdx: i})
	}
	if len(missingRows) > 0 && len(inputs) < totalShards {
		return nil, nil, nil, nil, rsec16.NotEnoughParityShardsError{}
	}

	return availableRows, missingRows, parityExponents, inputs, nil
}

// reconstructTo rebuilds the missing shards chunk range by chunk range,
// handing each rebuilt range to sink instead of accumulating it, so nothing
// reconstructed is ever resident beyond one chunk pass. preserved supplies
// surviving shards that were read before their source file was overwritten.
// sink is called once per chunk range and must consume its rows before returning.
func (d *Decoder) reconstructTo(cache *readerCache, preserved map[int][]byte, sink func(globalRows []int, off int, data [][]byte) error) ([]int, error) {
	availableRows, missingRows, parityExponents, inputs, err := d.classifyReconstruction()
	if err != nil {
		return nil, err
	}
	if len(missingRows) == 0 {
		return nil, nil
	}

	budget := d.memoryBudget
	if budget <= 0 {
		budget = min(defaultMemoryBudget(), defaultFoldMemoryBudget)
	}
	plan, err := rsec16.NewPAR2ReconstructionPlan(len(inputs), availableRows, missingRows, parityExponents, d.numGoroutines, budget)
	if err != nil {
		return nil, err
	}
	defer plan.Close()
	// Data inputs precede recovery inputs; preserved uses global data rows.
	chunk, err := plan.ChunkSize(d.sliceByteCount)
	if err != nil {
		return nil, err
	}
	out := make([][]byte, len(missingRows))

	for off := 0; off < d.sliceByteCount; off += chunk {
		n := chunk
		if off+n > d.sliceByteCount {
			n = d.sliceByteCount - off
		}
		for i := range out {
			if cap(out[i]) < n {
				out[i] = make([]byte, n)
			}
			out[i] = out[i][:n]
		}

		// Matrix plans may load inputs concurrently; the closure uses only
		// read-only plan state and the concurrency-safe cache.
		err := plan.Fold(n, func(j int, buf []byte) error {
			in := inputs[j]
			if in.isParity {
				loc := d.parityLocations[in.parityIdx]
				clear(buf)
				if off >= loc.length {
					return nil
				}
				end := off + n
				if end > loc.length {
					end = loc.length
				}
				return cache.readAbs(loc.path, loc.offset+int64(off), buf[:end-off])
			}
			if shard, ok := preserved[availableRows[j]]; ok {
				clear(buf)
				if off < len(shard) {
					end := off + n
					if end > len(shard) {
						end = len(shard)
					}
					copy(buf, shard[off:end])
				}
				return nil
			}
			return cache.readRange(in.shard, off, buf)
		}, out)
		if err != nil {
			return nil, err
		}

		if err := sink(missingRows, off, out); err != nil {
			return nil, err
		}
	}

	return missingRows, nil
}
