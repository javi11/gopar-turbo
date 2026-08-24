package par2

import (
	"errors"
	"io"

	"github.com/javi11/gopar-turbo/rsec16"
)

// parityLocation is where a recovery block's data lives on disk. Blocks are
// read back per chunk pass rather than retained: on a large set they are
// hundreds of megabytes in aggregate.
type parityLocation struct {
	path   string
	offset int64
	length int
}

// readerCache keeps data files open for the duration of a reconstruction.
// Reconstruction reads every surviving shard once per chunk pass, so reopening
// a file per shard would dominate the cost.
type readerCache struct {
	d       *Decoder
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

// chunkSizeFor returns the byte range of each slice to reconstruct per pass so
// that one accumulator per missing shard fits inside budget. The result is
// always even, which is all gf16 requires, and never exceeds a whole slice.
func chunkSizeFor(budget, sliceByteCount, missing int) int {
	if missing <= 0 {
		return sliceByteCount
	}
	if budget <= 0 {
		budget = defaultMemoryBudget()
	}
	per := budget / missing
	if per >= sliceByteCount {
		return sliceByteCount
	}
	per -= per % 2
	if per < 2 {
		per = 2
	}
	return per
}

// planReconstruction classifies every shard as available or missing and picks
// the recovery blocks that will stand in for the missing ones.
func (d *Decoder) planReconstruction() (rsec16.Coder, []int, []int, []int, []foldInput, error) {
	var availableRows, missingRows []int
	var inputs []foldInput

	totalShards := 0
	for _, info := range d.fileIntegrityInfos {
		totalShards += len(info.shardInfos)
	}
	if totalShards == 0 {
		return rsec16.Coder{}, nil, nil, nil, nil, errors.New("no file integrity info")
	}
	if len(d.parityPresent) == 0 {
		return rsec16.Coder{}, nil, nil, nil, nil, errors.New("no parity shards")
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

	var usedParityRows []int
	for i := 0; i < len(d.parityPresent) && len(inputs) < totalShards; i++ {
		if !d.parityPresent[i] {
			continue
		}
		usedParityRows = append(usedParityRows, i)
		inputs = append(inputs, foldInput{isParity: true, parityIdx: i})
	}
	if len(missingRows) > 0 && len(inputs) < totalShards {
		return rsec16.Coder{}, nil, nil, nil, nil, rsec16.NotEnoughParityShardsError{}
	}

	coder, err := rsec16.NewCoderPAR2Vandermonde(totalShards, len(d.parityPresent), d.numGoroutines)
	if err != nil {
		return rsec16.Coder{}, nil, nil, nil, nil, err
	}
	return coder, availableRows, missingRows, usedParityRows, inputs, nil
}

// reconstructMissing rebuilds every missing shard, keyed by its global shard
// index. It holds only one accumulator per missing shard — bounded further by
// MemoryBudget — and re-reads surviving shards from disk as it folds.
func (d *Decoder) reconstructMissing() (map[int][]byte, error) {
	coder, availableRows, missingRows, usedParityRows, inputs, err := d.planReconstruction()
	if err != nil {
		return nil, err
	}
	if len(missingRows) == 0 {
		return map[int][]byte{}, nil
	}

	m, err := coder.ReconstructionMatrix(availableRows, missingRows, usedParityRows)
	if err != nil {
		return nil, err
	}

	result := make(map[int][]byte, len(missingRows))
	for _, row := range missingRows {
		result[row] = make([]byte, d.sliceByteCount)
	}

	cache := newReaderCache(d)
	defer cache.Close()

	chunk := chunkSizeFor(d.memoryBudget, d.sliceByteCount, len(missingRows))
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

		err := rsec16.FoldInputs(m, len(inputs), n, func(j int, buf []byte) error {
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
			return cache.readRange(in.shard, off, buf)
		}, out, d.numGoroutines)
		if err != nil {
			return nil, err
		}

		for i, row := range missingRows {
			copy(result[row][off:off+n], out[i])
		}
	}

	return result, nil
}

// shardSource returns the bytes of the shard at the given global index,
// reading survivors from disk and taking rebuilt shards from reconstructed.
func (d *Decoder) shardSource(cache *readerCache, reconstructed map[int][]byte, globalIdx int, info shardIntegrityInfo, buf []byte) error {
	if shard, ok := reconstructed[globalIdx]; ok {
		clear(buf)
		copy(buf, shard)
		return nil
	}
	return cache.readRange(info, 0, buf)
}
