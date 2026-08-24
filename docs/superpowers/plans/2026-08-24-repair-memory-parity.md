# Repair Memory Parity Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Bring repair peak RSS from ~2.3 GB to ≤800 MB on the full-set benchmark (par2cmdline-turbo territory) without changing repair wall-clock or output bytes.

**Architecture:** Three memory eliminations in dependency order: recovery blocks are read from volumes by recorded offset instead of retained (−675 MB); the fold's accumulator footprint is capped at 256 MB via the existing chunking (measured free at 2 passes); reconstruction streams each chunk pass straight into pre-opened output files instead of accumulating reconstructed shards (−500 MB), with the final hash check streaming the written files.

**Tech Stack:** Go 1.26+, existing `walkPackets`/`readerCache`/`FoldInputs` machinery, `testify/require`, `memfs`.

## Global Constraints

- Output bytes identical to today on both backends; existing repair suites and realtool fixtures are the oracle.
- Repair wall-clock within noise of 9.02s / 16.66s (full set); every task passes `go test -race ./...` and `CGO_ENABLED=0 go test ./...`.
- Public API unchanged; `MemoryBudget` stays an upper bound (doc text may change, semantics of "lower it to use less" unchanged).
- No GC knobs in the library (`SetMemoryLimit`/GOGC are the consumer's).
- Stated behavior change (spec-approved, document on `Repair`): an interrupted/failed repair may leave a target file partially written.
- `gofmt -w` every touched file; pre-existing unformatted files stay untouched.

---

### Task 1: OpenWrite on the fileIO interface

**Files:**
- Modify: `par2/decoder.go` (`fileIO` interface ~line 19, `defaultFileIO`)
- Modify: `memfs/memfs.go`
- Modify: `par2/decoder_test.go` (`testFileIO` wrapper)
- Test: `par2/fileio_test.go` (extend)

**Interfaces:**
- Produces:
  ```go
  // OpenWrite creates or truncates path at the given size and returns a
  // positional writer plus a close function the caller must invoke.
  OpenWrite(path string, size int64) (io.WriterAt, func() error, error)
  ```
  Implemented by `defaultFileIO`, `memfs.MemFS`, `testFileIO`.

- [ ] **Step 1: Write the failing tests**

Append to `par2/fileio_test.go`:

```go
func TestMemFSOpenWritePositional(t *testing.T) {
	dir := memfs.RootDir()
	fs := memfs.MakeMemFS(dir, map[string][]byte{})
	path := filepath.Join(dir, "out.bin")

	w, closeFn, err := fs.OpenWrite(path, 8)
	require.NoError(t, err)
	_, err = w.WriteAt([]byte{5, 6}, 4) // out of order on purpose
	require.NoError(t, err)
	_, err = w.WriteAt([]byte{1, 2}, 0)
	require.NoError(t, err)
	require.NoError(t, closeFn())

	got, err := fs.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, []byte{1, 2, 0, 0, 5, 6, 0, 0}, got)
}

func TestMemFSOpenWriteTruncatesExisting(t *testing.T) {
	dir := memfs.RootDir()
	fs := memfs.MakeMemFS(dir, map[string][]byte{"out.bin": {9, 9, 9, 9, 9, 9}})
	path := filepath.Join(dir, "out.bin")

	w, closeFn, err := fs.OpenWrite(path, 3)
	require.NoError(t, err)
	_, err = w.WriteAt([]byte{1}, 0)
	require.NoError(t, err)
	require.NoError(t, closeFn())

	got, err := fs.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, []byte{1, 0, 0}, got)
}

func TestDefaultFileIOOpenWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.bin")

	w, closeFn, err := defaultFileIO{}.OpenWrite(path, 4)
	require.NoError(t, err)
	_, err = w.WriteAt([]byte{7, 8}, 2)
	require.NoError(t, err)
	require.NoError(t, closeFn())

	got, err := defaultFileIO{}.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, []byte{0, 0, 7, 8}, got)
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./par2/ -run 'OpenWrite' -count=1`
Expected: FAIL to compile — `fs.OpenWrite undefined`.

- [ ] **Step 3: Implement**

`par2/decoder.go` — add to `fileIO` and `defaultFileIO`:

```go
	// OpenWrite creates or truncates path at the given size and returns a
	// positional writer plus a close function the caller must invoke. It
	// lets repair write reconstructed chunk ranges directly to their final
	// offsets instead of assembling whole files in memory.
	OpenWrite(path string, size int64) (io.WriterAt, func() error, error)
```

```go
func (defaultFileIO) OpenWrite(path string, size int64) (io.WriterAt, func() error, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return nil, nil, err
	}
	if err := f.Truncate(size); err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, f.Close, nil
}
```

(0600 matches `defaultFileIO.WriteFile`'s existing permission choice.)

`memfs/memfs.go`:

```go
// memWriter writes into a fixed-size buffer registered under path at Close.
type memWriter struct {
	fs   MemFS
	path string
	data []byte
}

func (w *memWriter) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 || off+int64(len(p)) > int64(len(w.data)) {
		return 0, errors.New("memfs: write out of range")
	}
	copy(w.data[off:], p)
	return len(p), nil
}

// OpenWrite creates or truncates the file at path at the given size. Writes
// land in a private buffer that replaces the file's contents on close, so a
// concurrent reader of the old contents is unaffected, matching how the
// par2 package uses it (sources are pre-read before any OpenWrite).
func (fs MemFS) OpenWrite(path string, size int64) (io.WriterAt, func() error, error) {
	w := &memWriter{fs: fs, path: path, data: make([]byte, size)}
	closeFn := func() error {
		return fs.WriteFile(w.path, w.data)
	}
	return w, closeFn, nil
}
```

Add `"errors"` to memfs imports. `par2/decoder_test.go` — logging wrapper next to the others:

```go
func (io testFileIO) OpenWrite(path string, size int64) (w stdio.WriterAt, closeFn func() error, err error) {
	io.t.Helper()
	defer func() {
		io.t.Helper()
		io.t.Logf("OpenWrite(%s, %d) => %v", path, size, err)
	}()
	return io.fileIO.OpenWrite(path, size)
}
```

- [ ] **Step 4: Verify and commit**

Run: `go test ./par2/ -run OpenWrite -count=1 -v` → PASS ×3; `go build ./... && go vet ./...` clean.

```bash
gofmt -w par2/decoder.go par2/decoder_test.go par2/fileio_test.go memfs/memfs.go
git add par2/decoder.go par2/decoder_test.go par2/fileio_test.go memfs/memfs.go
git commit -m "feat: add positional OpenWrite to the fileIO interface"
```

---

### Task 2: Parity blocks on demand

**Files:**
- Modify: `par2/volume.go` (`walkPackets` callback gains body offset)
- Modify: `par2/decoder.go` (`Decoder` fields, `loadParityData` retain branch ~line 796)
- Modify: `par2/reconstruct.go` (`planReconstruction` ~line 119, parity input read ~line 209, `readerCache`)
- Modify: `par2/volume_test.go`, and the fold's parity path tests
- Test: `par2/volume_test.go` (extend)

**Interfaces:**
- Consumes: `walkPackets`, `readerCache`, recovery packet layout: block data begins at `packetOffset + 64 (header) + 4 (exponent prefix)`.
- Produces:
  ```go
  // parityLocation is where a recovery block's data lives on disk.
  type parityLocation struct {
      path   string // volume file
      offset int64  // absolute offset of the block data (past header+exponent)
      length int    // block length in bytes
  }
  ```
  `Decoder.parityLocations []parityLocation` (zero-valued entries = absent; `parityPresent` stays the presence source of truth). `d.parityShards` is **removed**.
  `readerCache` gains `getPath(path string) (io.ReaderAt, int64, error)` and `readAbs(path string, off int64, n int, buf []byte) error`; the fileID-keyed `get` delegates to it.

- [ ] **Step 1: Write the failing tests**

Append to `par2/volume_test.go`:

```go
// After LoadParityData, recovery-block bytes must not be retained; the
// location table must let the fold read them back exactly.
func TestLoadParityRecordsLocationsNotBytes(t *testing.T) {
	workingDir := memfs.RootDir()
	fs := makeShiftTestMemFS(workingDir)
	buildPAR2Data(t, fs, workingDir, 4, 2)

	d, err := newDecoderForTest(t, fs, filepath.Join(workingDir, "file.par2"))
	require.NoError(t, err)
	require.NoError(t, d.LoadFileData())
	require.NoError(t, d.LoadParityData())

	require.Equal(t, 2, d.ShardCounts().UsableParityShardCount)
	require.Len(t, d.parityLocations, 2)
	for i, loc := range d.parityLocations {
		require.NotEmpty(t, loc.path, "location %d", i)
		require.Equal(t, 4, loc.length, "location %d", i)

		// Round-trip: reading at the recorded offset yields the block that
		// a whole-file parse yields.
		r, _, closeFn, err := fs.OpenRead(loc.path)
		require.NoError(t, err)
		got := make([]byte, loc.length)
		_, err = r.ReadAt(got, loc.offset)
		require.NoError(t, err)
		require.NoError(t, closeFn())

		data, err := fs.ReadFile(loc.path)
		require.NoError(t, err)
		var want []byte
		buf := bytes.NewBuffer(data)
		for {
			_, typ, body, rerr := readNextPacket(buf)
			if rerr != nil {
				break
			}
			if typ == recoveryPacketType {
				exp, packet, perr := readRecoveryPacket(body)
				require.NoError(t, perr)
				if int(exp) == i {
					want = packet.data
				}
			}
		}
		require.Equal(t, want, got, "location %d must point at the block data", i)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./par2/ -run TestLoadParityRecordsLocations -count=1`
Expected: FAIL to compile — `d.parityLocations undefined`.

- [ ] **Step 3: Implement**

`par2/volume.go` — change the callback signature; the offset handed to `fn`
is the packet's start, from which the caller computes body positions:

```go
func walkPackets(r io.ReaderAt, size int64, fn func(setID recoverySetID, typ packetType, body []byte, packetOffset int64) error) error {
	// ... unchanged until the fn call:
		if err := fn(setID, typ, body, offset); err != nil {
			return err
		}
	// ...
}
```

Update the two `walkPackets` call sites in `par2/volume_test.go` (ignore the
new parameter) and the one in `loadParityData`.

`par2/decoder.go` — replace the `parityShards [][]byte` field with:

```go
	// parityLocations[i] is where recovery block i's data lives on disk;
	// blocks are read back on demand instead of being retained. Zero-valued
	// entries are absent — parityPresent is the source of truth.
	parityLocations []parityLocation
```

In `loadParityData`, the recovery case becomes (note `body` is
exponent-prefixed, so data starts 4 bytes in):

```go
				case recoveryPacketType:
					exp, packet, err := readRecoveryPacket(body)
					if err != nil {
						return err
					}
					delegate.OnRecoveryPacketLoad(uint16(exp), len(packet.data))
					if int(exp) >= len(parityPresent) {
						grow := int(exp+1) - len(parityPresent)
						parityPresent = append(parityPresent, make([]bool, grow)...)
						parityLocations = append(parityLocations, make([]parityLocation, grow)...)
					}
					parityPresent[exp] = true
					// First wins for duplicate exponents; duplicates are
					// byte-identical by packet hash.
					if retain && parityLocations[exp].path == "" {
						parityLocations[exp] = parityLocation{
							path:   match,
							offset: packetOffset + int64(sizeOfPacketHeader()) + 4,
							length: len(packet.data),
						}
					}
```

with `var parityLocations []parityLocation` replacing `var parityShards`,
and `d.parityLocations = parityLocations` at the end.

`par2/reconstruct.go`:

- `parityLocation` type declared here (it is reconstruction plumbing).
- `readerCache` generalizes to path-keyed readers:

```go
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

// readAbs fills buf from path at an absolute offset, zero-padding past end.
func (c *readerCache) readAbs(path string, off int64, buf []byte) error {
	clear(buf)
	r, size, err := c.getPath(path)
	if err != nil {
		return err
	}
	n := int64(len(buf))
	if off >= size {
		return nil
	}
	if off+n > size {
		n = size - off
	}
	_, err = r.ReadAt(buf[:n], off)
	return err
}
```

(Change the maps to `map[string]io.ReaderAt` / `map[string]int64`; the
fileID-keyed `get` resolves the path via `fileIDIndices` and calls `getPath`.)

- `planReconstruction`: `len(d.parityShards)` → `len(d.parityPresent)`;
  `len(d.parityShards[i]) == 0` → `!d.parityPresent[i]`; the coder is built
  with `len(d.parityPresent)` parity rows.
- The fold's parity input in `reconstructMissing` (and Task 4's successor)
  becomes:

```go
			if in.isParity {
				loc := d.parityLocations[in.parityIdx]
				end := off + n
				if end > loc.length {
					end = loc.length
				}
				if off >= loc.length {
					clear(buf)
					return nil
				}
				return cache.readAbs(loc.path, loc.offset+int64(off), buf[:end-off])
			}
```

(`readAbs` clears the buffer it is given; clear the full `buf` first when
truncating to `buf[:end-off]` so the tail stays zero: call `clear(buf)`
before the truncated `readAbs`.)

- `verifyParity`'s comparison loop reads each stored block into a scratch
  buffer via `readAbs` instead of indexing `d.parityShards`.
- `TestLoadParityDataRetainsBytes` in `par2/streaming_scan_test.go` asserted
  retention; rewrite it to assert locations are recorded and shards are not
  (fold correctness is covered by the repair suites).

- [ ] **Step 4: Verify and commit**

Run: `go test -race ./par2/ -count=1 && CGO_ENABLED=0 go test ./par2/ -count=1` → all PASS (repair suites prove byte-identical output).

```bash
gofmt -w par2/volume.go par2/decoder.go par2/reconstruct.go par2/volume_test.go par2/streaming_scan_test.go
git add par2 memfs
git commit -m "perf: read recovery blocks from volumes on demand

<paste: heap no longer shows loadParityData retention>"
```

---

### Task 3: Accumulator cap

**Files:**
- Modify: `par2/reconstruct.go` (`chunkSizeFor` ~line 99)
- Modify: `par2/repair.go`, `par2/verify.go` (`MemoryBudget` doc comment)
- Test: `par2/streaming_repair_test.go` (extend)

**Interfaces:**
- Produces: `chunkSizeFor` targeting `min(budget, foldAccumulatorCap)`; `const foldAccumulatorCap = 256 << 20`.

- [ ] **Step 1: Write the failing test**

Append to `par2/streaming_repair_test.go`:

```go
// The default accumulator footprint is capped: on a set where one
// accumulator per missing shard would exceed the cap, chunkSizeFor must
// split slices even with no explicit budget.
func TestChunkSizeForCapsDefaultFootprint(t *testing.T) {
	sliceByteCount := 2380956
	missing := 210 // the benchmark set's repair-missing shape: ~504 MB uncapped

	chunk := chunkSizeFor(0, sliceByteCount, missing)
	require.Less(t, chunk, sliceByteCount, "default must chunk when uncapped accumulators exceed the cap")
	require.LessOrEqual(t, chunk*missing, foldAccumulatorCap)
	require.Zero(t, chunk%2, "gf16 requires even sizes")

	// An explicit budget below the cap still wins.
	tight := chunkSizeFor(1<<20, sliceByteCount, missing)
	require.LessOrEqual(t, tight*missing, 1<<20)

	// Small sets stay single-pass.
	require.Equal(t, 4096, chunkSizeFor(0, 4096, 10))
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./par2/ -run TestChunkSizeForCaps -count=1`
Expected: FAIL — `undefined: foldAccumulatorCap` (and default returns the full slice).

- [ ] **Step 3: Implement**

In `par2/reconstruct.go`:

```go
// foldAccumulatorCap bounds the default accumulator footprint during
// reconstruction. Chunked passes are measured free at this size (2 passes on
// the benchmark set cost no wall-clock), so a larger MemoryBudget buys
// nothing and the cap applies regardless.
const foldAccumulatorCap = 256 << 20

func chunkSizeFor(budget, sliceByteCount, missing int) int {
	if missing <= 0 {
		return sliceByteCount
	}
	if budget <= 0 {
		budget = defaultMemoryBudget()
	}
	if budget > foldAccumulatorCap {
		budget = foldAccumulatorCap
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
```

Update the `MemoryBudget` doc comment in both options structs: replace the
"Zero selects half of physical memory" sentence with "Zero selects a default
capped at 256 MB of reconstruction accumulators; chunked passes at that size
are measured to cost no wall-clock. Lower values bound memory further at a
small time cost."

- [ ] **Step 4: Verify and commit**

Run: `go test -race ./par2/ -count=1 && CGO_ENABLED=0 go test ./par2/ -count=1` → PASS.

```bash
gofmt -w par2/reconstruct.go par2/repair.go par2/verify.go par2/streaming_repair_test.go
git add par2
git commit -m "perf: cap default reconstruction accumulators at 256 MB"
```

---

### Task 4: Incremental chunk writes

**Files:**
- Modify: `par2/reconstruct.go` (`reconstructMissing` → `reconstructTo`, `shardSource` retires)
- Modify: `par2/decoder.go` (`Repair` ~line 966, `verifyParity` call order, `preserveShardsBeforeOverwrite` signature, `Repair` doc comment)
- Test: `par2/streaming_repair_test.go` (extend)

**Interfaces:**
- Consumes: `OpenWrite` (Task 1), `parityLocations`/`readAbs` (Task 2), `chunkSizeFor` (Task 3).
- Produces:
  ```go
  // reconstructTo rebuilds the missing shards chunk range by chunk range,
  // handing each rebuilt range to sink instead of accumulating it. sink is
  // called once per (missing shard, chunk range), single-goroutine.
  func (d *Decoder) reconstructTo(cache *readerCache, preserved map[int][]byte, sink func(globalRow, off int, data []byte) error) (missingRows []int, err error)
  ```
  `preserved` replaces the old overloading of the reconstructed map for the
  swapped-files pre-read: it holds only cross-file surviving shards.

- [ ] **Step 1: Write the failing tests**

Append to `par2/streaming_repair_test.go`:

```go
// Repair must not buffer whole reconstructed files: with incremental writes
// the fileIO sees OpenWrite, not WriteFile, for repaired data files.
type writeRecordingFileIO struct {
	fileIO
	mu         sync.Mutex
	wholeFiles []string
	positional []string
}

func (w *writeRecordingFileIO) WriteFile(path string, data []byte) error {
	w.mu.Lock()
	w.wholeFiles = append(w.wholeFiles, filepath.Base(path))
	w.mu.Unlock()
	return w.fileIO.WriteFile(path, data)
}

func (w *writeRecordingFileIO) OpenWrite(path string, size int64) (io.WriterAt, func() error, error) {
	w.mu.Lock()
	w.positional = append(w.positional, filepath.Base(path))
	w.mu.Unlock()
	return w.fileIO.OpenWrite(path, size)
}

func TestRepairWritesIncrementally(t *testing.T) {
	workingDir := memfs.RootDir()
	fs := makeShiftTestMemFS(workingDir)
	buildPAR2Data(t, fs, workingDir, 4, 2)
	want := pristineBig(t, workingDir)
	perturbFile(t, fs, "big.rar")

	rec := &writeRecordingFileIO{fileIO: testFileIO{t, fs}}
	_, err := repair(rec, filepath.Join(workingDir, "file.par2"), RepairOptions{})
	require.NoError(t, err)

	require.Contains(t, rec.positional, "big.rar")
	require.NotContains(t, rec.wholeFiles, "big.rar",
		"repaired data files must be written positionally, not buffered whole")

	got, err := fs.ReadFile(filepath.Join(workingDir, "big.rar"))
	require.NoError(t, err)
	require.Equal(t, want, got)
}

// A tiny budget forces many chunk passes through the incremental writer;
// output must be unchanged, including the trailing partial slice.
func TestIncrementalRepairChunkedByteIdentical(t *testing.T) {
	workingDir := memfs.RootDir()
	fs := makeShiftTestMemFS(workingDir) // 40 bytes: 10 slices of 4
	buildPAR2Data(t, fs, workingDir, 4, 10)
	want := pristineBig(t, workingDir)
	_, err := fs.RemoveFile(filepath.Join(workingDir, "big.rar"))
	require.NoError(t, err)

	_, err = repair(testFileIO{t, fs}, filepath.Join(workingDir, "file.par2"),
		RepairOptions{MemoryBudget: 2}) // chunk = 2 bytes -> 2 passes per slice
	require.NoError(t, err)

	got, err := fs.ReadFile(filepath.Join(workingDir, "big.rar"))
	require.NoError(t, err)
	require.Equal(t, want, got)
}
```

Add `"io"` and `"sync"` to the test file's imports.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./par2/ -run 'TestRepairWritesIncrementally|TestIncrementalRepairChunked' -count=1`
Expected: `TestRepairWritesIncrementally` FAILS (`big.rar` appears in wholeFiles, not positional).

- [ ] **Step 3: Restructure reconstruction and Repair**

`par2/reconstruct.go` — replace `reconstructMissing` with `reconstructTo`.
The body keeps the existing plan/matrix/chunk-loop shape; the differences:
`result` map goes away, `preserved` supplies pre-read cross-file survivors to
the fold's data inputs, and each chunk's rows go to `sink`:

```go
func (d *Decoder) reconstructTo(cache *readerCache, preserved map[int][]byte, sink func(globalRow, off int, data []byte) error) ([]int, error) {
	coder, availableRows, missingRows, usedParityRows, inputs, err := d.planReconstruction()
	if err != nil {
		return nil, err
	}
	if len(missingRows) == 0 {
		return nil, nil
	}

	m, err := coder.ReconstructionMatrix(availableRows, missingRows, usedParityRows)
	if err != nil {
		return nil, err
	}

	// Map each available input to its global row so preserved shards can
	// stand in for sources whose files are being rewritten.
	inputRow := make([]int, len(inputs))
	for idx, row := range availableRows {
		inputRow[idx] = row
	}

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
			if shard, ok := preserved[inputRow[j]]; ok {
				clear(buf)
				copy(buf, shard[off:min(off+n, len(shard))])
				return nil
			}
			return cache.readRange(in.shard, off, buf)
		}, out, d.numGoroutines)
		if err != nil {
			return nil, err
		}

		for i, row := range missingRows {
			if err := sink(row, off, out[i]); err != nil {
				return nil, err
			}
		}
	}
	return missingRows, nil
}
```

Delete `shardSource` (its callers go away below).

`par2/decoder.go` — rewrite `Repair`:

```go
func (d *Decoder) Repair(checkParity bool) ([]string, error) {
	wasOK := make([]bool, len(d.fileIntegrityInfos))
	for i, info := range d.fileIntegrityInfos {
		wasOK[i] = info.ok(d.sliceByteCount)
	}

	cache := newReaderCache(d)
	defer cache.Close()

	// Reading shards on demand means rewriting one file can destroy the
	// source of another's shards (two files' contents swapped). Pre-read any
	// cross-file surviving shard whose source file is being rewritten,
	// before any file is opened for writing.
	preserved := make(map[int][]byte)
	if err := d.preserveShardsBeforeOverwrite(cache, preserved, wasOK); err != nil {
		return nil, err
	}

	// shardBase[i] is file i's first global shard row.
	shardBase := make([]int, len(d.recoverySet))
	{
		base := 0
		for i, info := range d.fileIntegrityInfos {
			shardBase[i] = base
			base += len(info.shardInfos)
		}
	}

	// Open a positional writer per file needing repair, and copy its
	// surviving shards to their canonical offsets. Reconstructed ranges
	// stream in afterwards, so no whole file or shard set is ever buffered.
	type openFile struct {
		file    int
		w       io.WriterAt
		closeFn func() error
	}
	writers := make(map[int]*openFile) // keyed by file index
	rowToFile := make(map[int]int)     // global missing row -> file index
	closeAll := func() {
		for _, of := range writers {
			if of.closeFn != nil {
				_ = of.closeFn()
			}
		}
	}

	shardBuf := make([]byte, d.sliceByteCount)
	for i, inputFileInfo := range d.recoverySet {
		if wasOK[i] {
			continue
		}
		path := d.getFilePath(inputFileInfo)
		w, closeFn, err := d.fileIO.OpenWrite(path, int64(inputFileInfo.byteCount))
		if err != nil {
			closeAll()
			return nil, err
		}
		of := &openFile{file: i, w: w, closeFn: closeFn}
		writers[i] = of

		info := d.fileIntegrityInfos[i]
		for j, si := range info.shardInfos {
			row := shardBase[i] + j
			if !si.present {
				rowToFile[row] = i
				continue
			}
			// Survivor: copy its bytes to the canonical offset now.
			if shard, ok := preserved[row]; ok {
				copy(shardBuf, shard)
			} else if err := cache.readRange(si, 0, shardBuf); err != nil {
				closeAll()
				return nil, err
			}
			if err := writeShardRange(w, j*d.sliceByteCount, shardBuf, inputFileInfo.byteCount); err != nil {
				closeAll()
				return nil, err
			}
		}
	}

	// Stream reconstruction straight into the open files.
	_, err := d.reconstructTo(cache, preserved, func(row, off int, data []byte) error {
		i, ok := rowToFile[row]
		if !ok {
			return errors.New("reconstructed a shard no file was waiting for")
		}
		info := d.recoverySet[i]
		fileOff := (row-shardBase[i])*d.sliceByteCount + off
		return writeShardRange(writers[i].w, fileOff, data, info.byteCount)
	})
	if err != nil {
		closeAll()
		return nil, err
	}

	// Close, then verify each rewritten file by streaming it back, exactly
	// as the scan hashes files. The file has already been replaced when a
	// mismatch is reported; see the Repair doc comment.
	var repairedPaths []string
	for i, inputFileInfo := range d.recoverySet {
		of, ok := writers[i]
		if !ok {
			continue
		}
		if err := of.closeFn(); err != nil {
			of.closeFn = nil
			closeAll()
			return repairedPaths, err
		}
		of.closeFn = nil

		path := d.getFilePath(inputFileInfo)
		err := d.checkWrittenFile(path, inputFileInfo)
		d.delegate.OnDataFileWrite(i+1, len(d.recoverySet), path, inputFileInfo.byteCount, err)
		if err != nil {
			return repairedPaths, err
		}
		repairedPaths = append(repairedPaths, path)

		info := d.fileIntegrityInfos[i]
		for j := range info.shardInfos {
			info.shardInfos[j] = shardIntegrityInfo{
				present:   true,
				foundAt:   shardLocation{info.fileID, j * d.sliceByteCount},
				locations: shardLocationSet{{info.fileID, j * d.sliceByteCount}: true},
			}
		}
		info.missing = false
		info.hashMismatch = false
		info.hasWrongByteCount = false
		d.fileIntegrityInfos[i] = info
	}

	if checkParity {
		// Every shard now sits at its canonical offset on disk, so the
		// parity double-check streams from the repaired files.
		if err := d.verifyParity(cache); err != nil {
			return repairedPaths, err
		}
	}

	// TODO: Repair missing parity volumes, too, and then make
	// sure d.Verify() passes.

	return repairedPaths, nil
}

// writeShardRange writes data at fileOff, clipping at byteCount: the last
// shard of a file is zero-padded in memory but not on disk.
func writeShardRange(w io.WriterAt, fileOff int, data []byte, byteCount int) error {
	if fileOff >= byteCount {
		return nil
	}
	n := len(data)
	if fileOff+n > byteCount {
		n = byteCount - fileOff
	}
	_, err := w.WriteAt(data[:n], int64(fileOff))
	return err
}

// checkWrittenFile streams a repaired file and verifies its 16k hash and MD5.
func (d *Decoder) checkWrittenFile(path string, info decoderInputFileInfo) error {
	r, size, closeFn, err := d.fileIO.OpenRead(path)
	if err != nil {
		return err
	}
	defer closeFn()
	if int(size) != info.byteCount {
		return errors.New("wrong byte count in reconstructed data")
	}

	fullHash := md5.New()
	head := make([]byte, 0, 16*1024)
	buf := make([]byte, 1<<20)
	for off := int64(0); off < size; {
		n := len(buf)
		if int64(n) > size-off {
			n = int(size - off)
		}
		if _, err := r.ReadAt(buf[:n], off); err != nil {
			return err
		}
		fullHash.Write(buf[:n])
		if len(head) < cap(head) {
			head = append(head, buf[:min(n, cap(head)-len(head))]...)
		}
		off += int64(n)
	}
	if md5.Sum(head) != info.sixteenKHash {
		return errors.New("hash mismatch (16k) in reconstructed data")
	}
	var full [md5.Size]byte
	copy(full[:], fullHash.Sum(nil))
	if full != info.hash {
		return errors.New("hash mismatch in reconstructed data")
	}
	return nil
}
```

`preserveShardsBeforeOverwrite` keeps its logic but writes into the passed
`preserved` map (rename its `reconstructed` parameter; drop the
`if _, done := reconstructed[idx]; done` short-circuit's dependence on
reconstruction — it now only skips already-preserved rows).

`verifyParity` changes signature to `(cache *readerCache) error`: it reads
every data shard from disk via `cache.readRange` (all shards are canonical
post-repair) and every stored parity block via `readAbs` on
`d.parityLocations`, comparing per chunk as it already does. Its
`reconstructed`-map plumbing goes away.

Add to `Repair`'s doc comment (and mirror one sentence on `par2.Repair`):
"Repaired files are written incrementally; if repair fails or is
interrupted partway, a target file may be left partially written."

- [ ] **Step 4: Verify**

Run: `go test -race ./... -count=1 && CGO_ENABLED=0 go test ./... -count=1 && go vet ./...`
Expected: all PASS — including `TestRepairSwappedFiles` (the pre-read now
feeds both the survivor copy and the fold), `TestRealTool*`, and the
DoubleCheck path (`RepairOptions{DoubleCheck: true}` in `realtool_test.go`).

- [ ] **Step 5: Commit**

```bash
gofmt -w par2/reconstruct.go par2/decoder.go par2/streaming_repair_test.go
git add par2
git commit -m "perf: stream reconstructed chunks straight into output files

<paste heap/RSS numbers>"
```

---

### Task 5: Measure, document, re-bench

**Files:**
- Modify: `bench/RESULTS.md`, `bench/results.csv`, `README.md` (Memory section)

- [ ] **Step 1: Spot-check the full set**

```bash
SP=<session scratchpad>
go build -o "$SP/bin/par2bench-cgo" ./cmd/par2bench/
# repair-missing and repair-corrupt one-offs as in earlier tasks
```
Expected: repair RSS ≤ 800 MB, time within noise of 9.02s / 16.66s, output
MD5-identical to pristine. If RSS misses, heap-profile before changing
anything.

- [ ] **Step 2: Full sweeps**

Archive `results.csv` (e.g. `results_prememfix.csv`), rerun the scaled
(reps 3) and full (reps 1) sweeps exactly as before, all rows `md5=ok`.

- [ ] **Step 3: Update documents and commit**

RESULTS.md: new memory numbers in both tables, update "Where each tool still
wins" (memory should move from a par2cmdline win to rough parity, with the
honest Go-GC-headroom caveat), refresh `bench/results.csv`. README Memory
section: mention blocks-on-demand and incremental writes in one sentence each.

```bash
go test -count=1 -race ./... && CGO_ENABLED=0 go test -count=1 ./...
git add bench/RESULTS.md bench/results.csv README.md
git commit -m "docs: benchmark results after repair memory work"
```

---

## Self-Review

**Spec coverage.** Parity-on-demand → Task 2 (locations, readAbs, verifyParity).
Accumulator cap → Task 3. Incremental writes, streamed hash check, swapped-files
pre-read ordering, DoubleCheck post-write → Task 4. OpenWrite contract → Task 1.
Behavior-change documentation → Task 4 Step 3. Bench + docs → Task 5.

**Placeholder scan.** Commit messages contain paste-markers for measured
numbers — intentional (numbers don't exist until run). No TBDs.

**Type consistency.** `OpenWrite(path, size) (io.WriterAt, func() error, error)`
consistent across Task 1 and Task 4. `parityLocation{path, offset, length}` and
`readAbs(path, off, buf)` consistent across Tasks 2 and 4. `reconstructTo(cache,
preserved, sink)` defined and consumed in Task 4. `preserved map[int][]byte`
threads `preserveShardsBeforeOverwrite` → survivor copy → fold inputs.
`min` is a Go 1.21+ builtin (repo requires 1.26+).
