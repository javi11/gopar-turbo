package par2

import (
	"crypto/md5"
	"errors"
	"io"
	"sync"
	"sync/atomic"
)

// windowSource is one file to be walked in slice-aligned windows. A nil
// reader means the file is absent and is skipped.
type windowSource struct {
	r    io.ReaderAt
	size int64
}

// forEachWindow reads every source in slice-aligned windows of windowSize
// bytes and calls fn(i, off, buf) for each, from a pool of workers, where
// buf holds the window's bytes and off is its file offset. fn must be safe
// for concurrent use. Windows rather than files are the unit of work so
// that a few large files still spread across every core, and so that on
// asymmetric CPUs a whole file is never pinned to one slow core. The
// result holds the first error seen per source.
func forEachWindow(sources []windowSource, windowSize, workers int, fn func(i, off int, buf []byte) error) []error {
	type task struct {
		i   int
		off int64
		n   int
	}
	errs := make([]error, len(sources))
	var errMu sync.Mutex
	setErr := func(i int, err error) {
		errMu.Lock()
		if errs[i] == nil {
			errs[i] = err
		}
		errMu.Unlock()
	}

	tasks := make(chan task)
	var wg sync.WaitGroup
	wg.Add(max(workers, 1))
	for w := 0; w < max(workers, 1); w++ {
		go func() {
			defer wg.Done()
			buf := make([]byte, windowSize)
			for t := range tasks {
				if errs[t.i] != nil {
					continue
				}
				if _, err := sources[t.i].r.ReadAt(buf[:t.n], t.off); err != nil {
					setErr(t.i, err)
					continue
				}
				if err := fn(t.i, int(t.off), buf[:t.n]); err != nil {
					setErr(t.i, err)
				}
			}
		}()
	}
	for i, src := range sources {
		if src.r == nil {
			continue
		}
		for off := int64(0); off < src.size; off += int64(windowSize) {
			n := int64(windowSize)
			if off+n > src.size {
				n = src.size - off
			}
			tasks <- task{i, off, int(n)}
		}
	}
	close(tasks)
	wg.Wait()
	return errs
}

// scanCounts accumulates a file's scan tallies across windows.
type scanCounts struct {
	hits, misses atomic.Int64
	sixteenK     [md5.Size]byte
}

// scanFilesWindowed is the default scan: every present file is walked in
// slice-aligned windows across a shared worker pool, and each window's
// slices are matched against the IFSC checksums at their canonical
// positions. It records the 16k hash from each file's first window and
// returns per-file counts and the first per-file error. Sources with a
// nil reader are missing and are left untouched.
func (d *Decoder) scanFilesWindowed(sources []windowSource, checksumToLocation checksumShardLocationMap, fileIntegrityInfos []fileIntegrityInfo, fileIDIndices map[fileID]int) ([]scanCounts, []error) {
	counts := make([]scanCounts, len(sources))
	windowSize := scanWindowSize(d.sliceByteCount)
	errs := forEachWindow(sources, windowSize, d.numGoroutines, func(i, off int, buf []byte) error {
		if off == 0 {
			counts[i].sixteenK = sixteenKHash(buf)
		}
		_, h, m := scanBuffer(d.sliceByteCount, buf, len(buf), off,
			checksumToLocation, d.recoverySet[i].fileID, fileIntegrityInfos, fileIDIndices, d.scanPolicy, &d.scanMu)
		counts[i].hits.Add(int64(h))
		counts[i].misses.Add(int64(m))
		return nil
	})
	for i, src := range sources {
		if src.r != nil && src.size == 0 {
			counts[i].sixteenK = md5.Sum(nil)
		}
	}
	return counts, errs
}

var (
	errWrittenByteCount = errors.New("wrong byte count in reconstructed data")
	errWritten16k       = errors.New("hash mismatch (16k) in reconstructed data")
	errWrittenSlice     = errors.New("hash mismatch in reconstructed data")
)

// checkWrittenFiles verifies repaired files by reading them back and
// matching slices' MD5s against the IFSC packet at their canonical index,
// plus the 16k hash and byte count. paths[i] == "" skips file i. rows[i]
// lists the slice indices to check for file i; nil means every slice, in
// which case the file is walked in windows across the worker pool so the
// check runs at slice rather than file granularity. Returns one error per
// file.
func (d *Decoder) checkWrittenFiles(paths []string, rows [][]int) []error {
	sources := make([]windowSource, len(paths))
	errs := make([]error, len(paths))
	var closers []func() error
	for i, path := range paths {
		if path == "" {
			continue
		}
		r, size, closeFn, err := d.fileIO.OpenRead(path)
		if err != nil {
			errs[i] = err
			continue
		}
		closers = append(closers, closeFn)
		if int(size) != d.recoverySet[i].byteCount {
			errs[i] = errWrittenByteCount
			continue
		}
		sources[i] = windowSource{r, size}
	}
	defer func() {
		for _, c := range closers {
			_ = c()
		}
	}()

	full := make([]windowSource, len(paths))
	for i, src := range sources {
		if src.r != nil && rows[i] == nil {
			full[i] = src
		}
	}
	windowSize := scanWindowSize(d.sliceByteCount)
	winErrs := forEachWindow(full, windowSize, d.numGoroutines, func(i, off int, buf []byte) error {
		info := d.recoverySet[i]
		if off == 0 && sixteenKHash(buf) != info.sixteenKHash {
			return errWritten16k
		}
		for j := 0; j < len(buf); j += d.sliceByteCount {
			if err := d.checkSlice(info, (off+j)/d.sliceByteCount, sliceAndPadByteArray(buf, j, j+d.sliceByteCount)); err != nil {
				return err
			}
		}
		return nil
	})
	for i, err := range winErrs {
		if err != nil && errs[i] == nil {
			errs[i] = err
		}
	}

	// Partially rewritten files: check the listed slices and the 16k head.
	type task struct{ i, idx int }
	tasks := make(chan task)
	var mu sync.Mutex
	var wg sync.WaitGroup
	workers := max(d.numGoroutines, 1)
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			buf := make([]byte, d.sliceByteCount)
			for t := range tasks {
				err := d.checkSliceAt(sources[t.i], d.recoverySet[t.i], t.idx, buf)
				if err != nil {
					mu.Lock()
					if errs[t.i] == nil {
						errs[t.i] = err
					}
					mu.Unlock()
				}
			}
		}()
	}
	for i, src := range sources {
		if src.r == nil || rows[i] == nil || errs[i] != nil {
			continue
		}
		tasks <- task{i, -1}
		for _, idx := range rows[i] {
			tasks <- task{i, idx}
		}
	}
	close(tasks)
	wg.Wait()

	// A zero-length file has no windows: check its 16k hash directly.
	for i, src := range sources {
		if src.r != nil && src.size == 0 && errs[i] == nil && md5.Sum(nil) != d.recoverySet[i].sixteenKHash {
			errs[i] = errWritten16k
		}
	}
	return errs
}

// checkSlice matches one padded slice against the IFSC MD5 at idx.
func (d *Decoder) checkSlice(info decoderInputFileInfo, idx int, slice []byte) error {
	if idx >= len(info.checksumPairs) || md5.Sum(slice) != info.checksumPairs[idx].MD5 {
		return errWrittenSlice
	}
	return nil
}

// checkSliceAt reads slice idx of src (idx -1 means the 16k head) into buf
// and checks it. Bytes past the end of the file read as zero padding.
func (d *Decoder) checkSliceAt(src windowSource, info decoderInputFileInfo, idx int, buf []byte) error {
	if idx < 0 {
		n := min(int64(16*1024), src.size)
		head := buf[:n]
		if _, err := src.r.ReadAt(head, 0); err != nil && n > 0 {
			return err
		}
		if md5.Sum(head) != info.sixteenKHash {
			return errWritten16k
		}
		return nil
	}
	clear(buf)
	start := int64(idx) * int64(d.sliceByteCount)
	if start > src.size {
		return errWrittenSlice
	}
	n := min(int64(d.sliceByteCount), src.size-start)
	if n > 0 {
		if _, err := src.r.ReadAt(buf[:n], start); err != nil {
			return err
		}
	}
	return d.checkSlice(info, idx, buf)
}
