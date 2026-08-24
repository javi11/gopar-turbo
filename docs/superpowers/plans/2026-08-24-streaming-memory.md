# Bounded-Memory Verify and Repair Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `gopar-turbo` verify in memory independent of set size, and repair within an explicit memory budget, without changing output bytes.

**Architecture:** Two phases. The scan walks each file through a small sliding window and records shard *presence and location* instead of shard bytes, so verify never materialises the set. Repair then folds input shards into accumulators for the missing shards only, pulling each input from disk on demand, so peak memory is `missing × sliceSize` rather than the whole set.

**Tech Stack:** Go 1.26+, cgo ParPar GF(2^16) kernels (`gf16`), pure-Go fallback (`gf2p16`), `testify/require`, `memfs` for in-memory test filesystems.

## Global Constraints

- Output must be **bit-identical** to today on both backends. `Coder.ReconstructData` stays as the equivalence oracle.
- Every change must pass with `CGO_ENABLED=1` and `CGO_ENABLED=0`, and under `-race`.
- Public API changes are **additive only** — the gopar-compatible shapes of `VerifyOptions`, `RepairOptions`, `NewDecoder`, and `Verify`/`Repair` must not change.
- `gf16` buffer sizes must satisfy `gf16.NewContext`: any chunk size passed to a context must be even and a multiple of `ctx.Stride()`.
- Slice size in the real benchmark set is `2380956`; do not hard-code slice sizes anywhere.
- Run `gofmt -w` on every file you touch. `par2/crc32_test.go` is already unformatted on main — leave it alone.

---

### Task 1: Streaming read on the fileIO interface

**Files:**
- Modify: `par2/decoder.go` (the `fileIO` interface and `defaultFileIO`, lines 18-36)
- Modify: `memfs/memfs.go`
- Modify: `par2/decoder_test.go` (`testFileIO` wrapper, around line 80)
- Test: `par2/fileio_test.go` (create)

**Interfaces:**
- Consumes: nothing.
- Produces:
  ```go
  type readerAtCloser interface {
      io.ReaderAt
      io.Closer
  }
  // OpenRead returns a reader over path plus its size in bytes. The caller
  // must Close the reader.
  OpenRead(path string) (readerAtCloser, int64, error)
  ```
  Implemented by `defaultFileIO`, `memfs.MemFS`, and `testFileIO`.

- [ ] **Step 1: Write the failing test**

Create `par2/fileio_test.go`:

```go
package par2

import (
	"path/filepath"
	"testing"

	"github.com/javi11/gopar-turbo/memfs"
	"github.com/stretchr/testify/require"
)

func TestMemFSOpenReadReturnsSizeAndBytes(t *testing.T) {
	dir := memfs.RootDir()
	fs := memfs.MakeMemFS(dir, map[string][]byte{
		"a.bin": {0x1, 0x2, 0x3, 0x4, 0x5},
	})

	r, size, err := fs.OpenRead(filepath.Join(dir, "a.bin"))
	require.NoError(t, err)
	defer r.Close()
	require.Equal(t, int64(5), size)

	buf := make([]byte, 3)
	n, err := r.ReadAt(buf, 1)
	require.NoError(t, err)
	require.Equal(t, 3, n)
	require.Equal(t, []byte{0x2, 0x3, 0x4}, buf)
}

func TestMemFSOpenReadMissingFile(t *testing.T) {
	dir := memfs.RootDir()
	fs := memfs.MakeMemFS(dir, map[string][]byte{})
	_, _, err := fs.OpenRead(filepath.Join(dir, "nope.bin"))
	require.Error(t, err)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./par2/ -run TestMemFSOpenRead -v`
Expected: FAIL to compile — `fs.OpenRead undefined (type memfs.MemFS has no field or method OpenRead)`.

- [ ] **Step 3: Write minimal implementation**

In `par2/decoder.go`, add `io` to the imports, then define the interface and extend `fileIO`:

```go
// readerAtCloser is a random-access reader that must be closed.
type readerAtCloser interface {
	io.ReaderAt
	io.Closer
}

type fileIO interface {
	ReadFile(path string) ([]byte, error)
	// OpenRead returns a reader over path plus its size in bytes, so a
	// caller can walk a large file without materialising it. The caller
	// must Close the returned reader.
	OpenRead(path string) (readerAtCloser, int64, error)
	FindWithPrefixAndSuffix(prefix, suffix string) ([]string, error)
	WriteFile(path string, data []byte) error
}

func (io2 defaultFileIO) OpenRead(path string) (readerAtCloser, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, fi.Size(), nil
}
```

In `memfs/memfs.go`, add `bytes` and `io` to imports and append:

```go
// memReader adapts a byte slice to the random-access reader the par2
// package expects. Closing it is a no-op.
type memReader struct{ *bytes.Reader }

func (memReader) Close() error { return nil }

// OpenRead returns a reader over the file at path plus its size.
func (fs MemFS) OpenRead(path string) (interface {
	io.ReaderAt
	io.Closer
}, int64, error) {
	data, err := fs.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	return memReader{bytes.NewReader(data)}, int64(len(data)), nil
}
```

In `par2/decoder_test.go`, add the logging wrapper next to the others:

```go
func (io testFileIO) OpenRead(path string) (r readerAtCloser, size int64, err error) {
	io.t.Helper()
	defer func() {
		io.t.Helper()
		io.t.Logf("OpenRead(%s) => (%d bytes, %v)", path, size, err)
	}()
	return io.fileIO.OpenRead(path)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./par2/ ./memfs/ -run 'TestMemFSOpenRead' -v`
Expected: PASS for both tests.

Run: `go build ./... && go vet ./...`
Expected: no output.

- [ ] **Step 5: Commit**

```bash
gofmt -w par2/decoder.go par2/decoder_test.go par2/fileio_test.go memfs/memfs.go
git add par2/decoder.go par2/decoder_test.go par2/fileio_test.go memfs/memfs.go
git commit -m "feat: add streaming OpenRead to the fileIO interface"
```

---

### Task 2: Scan records shard presence and location, not bytes

This is the change that fixes verify. `shardIntegrityInfo` stops carrying a payload.

**Files:**
- Modify: `par2/decoder.go` (`shardIntegrityInfo` ~line 124, `ok` ~line 130, `fillShardInfos` ~line 338, `ShardCounts` ~line 673, repair write path ~line 755)
- Test: `par2/streaming_scan_test.go` (create)

**Interfaces:**
- Consumes: `OpenRead` from Task 1.
- Produces:
  ```go
  type shardIntegrityInfo struct {
      present   bool
      foundAt   shardLocation      // where the bytes actually live on disk
      locations shardLocationSet
  }
  ```
  Later tasks read `foundAt` to re-read a shard during reconstruction.

- [ ] **Step 1: Write the failing test**

Create `par2/streaming_scan_test.go`:

```go
package par2

import (
	"path/filepath"
	"testing"

	"github.com/javi11/gopar-turbo/memfs"
	"github.com/stretchr/testify/require"
)

// Verify must report shard presence without retaining shard payloads.
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

	counts := 0
	for _, si := range d.fileIntegrityInfos[0].shardInfos {
		if !si.present {
			counts++
		}
	}
	require.Equal(t, 1, counts, "exactly the perturbed shard should be absent")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./par2/ -run 'TestScanRecords|TestScanMarks' -v`
Expected: FAIL to compile — `si.present undefined` and `si.foundAt undefined`.

- [ ] **Step 3: Write minimal implementation**

In `par2/decoder.go` replace the struct and its predicate:

```go
type shardIntegrityInfo struct {
	// present is true when the shard's bytes were located on disk.
	present bool
	// foundAt is where those bytes actually live, which is not
	// necessarily the shard's canonical offset.
	foundAt   shardLocation
	locations shardLocationSet
}

func (info shardIntegrityInfo) ok(location shardLocation) bool {
	return info.present && info.locations[location]
}
```

In `fillShardInfos`, replace the block that stored the payload:

```go
		location := shardLocation{fileID, j}
		for foundLocation := range foundLocations {
			integrityInfo := fileIntegrityInfos[fileIDIndices[foundLocation.fileID]]
			shardInfo := &integrityInfo.shardInfos[foundLocation.start/sliceByteCount]
			if !shardInfo.present {
				*shardInfo = shardIntegrityInfo{
					present:   true,
					foundAt:   location,
					locations: shardLocationSet{},
				}
			}
			shardInfo.locations[location] = true
		}
```

In `ShardCounts`, replace `if shardInfo.data == nil {` with `if !shardInfo.present {`.

There is a second `shardInfo.data == nil` test in the repair path (around line 673) — replace it with `!shardInfo.present` as well.

The repair write path at line ~755 still needs bytes and is rewritten in Task 6. To keep the tree green in the meantime, give the decoder a helper that re-reads a shard, and use it there:

```go
// readShard fills buf with the bytes of the given shard, re-reading them
// from wherever the scan found them. buf must be sliceByteCount long.
func (d *Decoder) readShard(info shardIntegrityInfo, buf []byte) error {
	if !info.present {
		return errors.New("shard not present")
	}
	idx, ok := d.fileIDIndices[info.foundAt.fileID]
	if !ok {
		return errors.New("unknown file for shard")
	}
	path := d.getFilePath(d.recoverySet[idx])
	r, size, err := d.fileIO.OpenRead(path)
	if err != nil {
		return err
	}
	defer r.Close()

	for i := range buf {
		buf[i] = 0
	}
	end := int64(info.foundAt.start + len(buf))
	if end > size {
		end = size
	}
	n := int(end) - info.foundAt.start
	if n <= 0 {
		return nil
	}
	_, err = r.ReadAt(buf[:n], int64(info.foundAt.start))
	return err
}
```

Add `fileIDIndices map[fileID]int` to the `Decoder` struct and populate it in `LoadFileData` alongside the existing local map, so `readShard` can resolve a file ID.

In the repair write loop, replace the `binary.Write(buf, ..., shardInfo.data)` line with:

```go
		shardBuf := make([]byte, d.sliceByteCount)
		buf := bytes.NewBuffer(nil)
		for _, shardInfo := range fileIntegrityInfo.shardInfos {
			if err := d.readShard(shardInfo, shardBuf); err != nil {
				return repairedPaths, err
			}
			if _, err := buf.Write(shardBuf); err != nil {
				return repairedPaths, err
			}
		}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./par2/ -run 'TestScanRecords|TestScanMarks' -v`
Expected: PASS.

Run: `go test -race ./... && CGO_ENABLED=0 go test ./...`
Expected: all packages `ok`.

- [ ] **Step 5: Commit**

```bash
gofmt -w par2/decoder.go par2/streaming_scan_test.go
git add par2/decoder.go par2/streaming_scan_test.go
git commit -m "refactor: record shard presence and location instead of payloads"
```

---

### Task 3: Windowed file scan

Replace the whole-file `ReadFile` in the scan with a sliding window so verify memory stops tracking set size.

**Files:**
- Modify: `par2/decoder.go` (`fillFileIntegrityInfos` ~line 387)
- Test: `par2/streaming_scan_test.go` (extend)

**Interfaces:**
- Consumes: `OpenRead` (Task 1), `shardIntegrityInfo.present/foundAt` (Task 2).
- Produces: `scanFile(path string, info decoderInputFileInfo, ...) (byteCount, hits, misses int, err error)` — same return shape `fillFileIntegrityInfos` has today, so callers are unchanged.

- [ ] **Step 1: Write the failing test**

Append to `par2/streaming_scan_test.go`:

```go
// The scan must never allocate a buffer proportional to the file. We assert
// on behaviour rather than allocation: a file much larger than the window
// still scans correctly, including its trailing partial slice.
func TestScanHandlesFileLargerThanWindow(t *testing.T) {
	workingDir := memfs.RootDir()
	sliceByteCount := 16
	data := make([]byte, sliceByteCount*40+7) // 41 slices, last partial
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
```

- [ ] **Step 2: Run test to verify it fails or passes for the wrong reason**

Run: `go test ./par2/ -run TestScanHandlesFileLargerThanWindow -v`
Expected: PASS (today's whole-file read handles this). This test is a **regression guard** for Task 3 — it must keep passing after the rewrite. Record that it passes now, then proceed.

- [ ] **Step 3: Write the implementation**

Rewrite `fillFileIntegrityInfos` to stream. Keep the existing signature and return values so no caller changes:

```go
func (d *Decoder) fillFileIntegrityInfos(checksumToLocation checksumShardLocationMap, fileIntegrityInfos []fileIntegrityInfo, fileIDIndices map[fileID]int, i int, info decoderInputFileInfo) (int, int, int, error) {
	path := d.getFilePath(info)
	r, size, err := d.fileIO.OpenRead(path)
	if os.IsNotExist(err) {
		fileIntegrityInfos[i].missing = true
		return 0, 0, 0, nil
	} else if err != nil {
		return 0, 0, 0, err
	}
	defer r.Close()

	// Two slices is the smallest window that lets the misaligned search
	// slide a full slice past a boundary without re-reading.
	windowSize := 2 * d.sliceByteCount
	buf := make([]byte, windowSize)

	fullHash := md5.New()
	var sixteenK [md5.Size]byte
	var sixteenKDone bool

	hits, misses := 0, 0
	var carry []byte
	var offset int64

	for offset < size {
		n := windowSize - len(carry)
		if int64(n) > size-offset {
			n = int(size - offset)
		}
		copy(buf, carry)
		if _, err := r.ReadAt(buf[len(carry):len(carry)+n], offset); err != nil {
			return int(size), hits, misses, err
		}
		chunk := buf[:len(carry)+n]
		fullHash.Write(buf[len(carry) : len(carry)+n])
		if !sixteenKDone && offset+int64(n) >= 16*1024 {
			// computed separately below from the first 16k
			sixteenKDone = true
		}
		offset += int64(n)

		// Scan whole slices out of the window, leaving a tail as carry.
		consumed, h, m := scanWindow(d, chunk, checksumToLocation, info.fileID,
			fileIntegrityInfos, fileIDIndices, offset == size, int(offset)-len(chunk))
		hits += h
		misses += m
		carry = append(carry[:0], chunk[consumed:]...)
	}

	// The 16k hash needs the first 16k, which is cheap to re-read.
	sixteenK = d.sixteenKHashOf(r, size)

	var full [md5.Size]byte
	copy(full[:], fullHash.Sum(nil))

	hashMismatch := sixteenK != info.sixteenKHash || full != info.hash
	fileIntegrityInfos[i].hashMismatch = hashMismatch
	if hashMismatch {
		d.delegate.OnDetectDataFileHashMismatch(info.fileID, path)
	}

	hasWrongByteCount := int(size) != info.byteCount
	fileIntegrityInfos[i].hasWrongByteCount = hasWrongByteCount
	if hasWrongByteCount {
		d.delegate.OnDetectDataFileWrongByteCount(info.fileID, path)
	}

	return int(size), hits, misses, nil
}

// sixteenKHashOf returns the PAR2 16k hash for the file, which is the MD5 of
// the first 16 KiB, or of the whole file when it is shorter.
func (d *Decoder) sixteenKHashOf(r readerAtCloser, size int64) [md5.Size]byte {
	n := int64(16 * 1024)
	if size < n {
		n = size
	}
	head := make([]byte, n)
	if n > 0 {
		if _, err := r.ReadAt(head, 0); err != nil {
			return [md5.Size]byte{}
		}
	}
	return md5.Sum(head)
}
```

Extract the existing loop body of `fillShardInfos` into `scanWindow`, which scans a window and returns how many bytes it consumed. `baseOffset` is the file offset of `chunk[0]`, so recorded locations stay absolute:

```go
// scanWindow scans chunk for known shards and returns the number of leading
// bytes it fully consumed, plus hit and miss counts. When atEOF is false it
// leaves up to one slice unconsumed so the next window can carry it.
func scanWindow(d *Decoder, chunk []byte, checksumToLocation checksumShardLocationMap, id fileID, fileIntegrityInfos []fileIntegrityInfo, fileIDIndices map[fileID]int, atEOF bool, baseOffset int) (int, int, int) {
	limit := len(chunk)
	if !atEOF {
		limit -= d.sliceByteCount
		if limit < 0 {
			limit = 0
		}
	}
	// Reuse the existing scanner over the window, offsetting recorded
	// locations by baseOffset.
	hits, misses := fillShardInfosAt(d.sliceByteCount, chunk, limit, baseOffset,
		checksumToLocation, id, fileIntegrityInfos, fileIDIndices, d.scanPolicy)
	return limit, hits, misses
}
```

Rename the existing `fillShardInfos` to `fillShardInfosAt`, adding `limit` and `baseOffset` parameters: the loop condition becomes `for j := 0; j < limit;` and every `shardLocation{fileID, j}` becomes `shardLocation{fileID, baseOffset + j}`. Keep a thin `fillShardInfos` wrapper with the old signature so `par2/decoder_test.go` and `par2/misaligned_test.go` keep compiling:

```go
func fillShardInfos(sliceByteCount int, data []byte, checksumToLocation checksumShardLocationMap, fileID fileID, fileIntegrityInfos []fileIntegrityInfo, fileIDIndices map[fileID]int, policy scanPolicy) (int, int) {
	return fillShardInfosAt(sliceByteCount, data, len(data), 0, checksumToLocation, fileID, fileIntegrityInfos, fileIDIndices, policy)
}
```

- [ ] **Step 4: Run the full suite**

Run: `go test ./par2/ -v -run 'TestScan|TestFillShardInfos|TestRepair|TestVerify|TestRealTool'`
Expected: PASS.

Run: `go test -race ./... && CGO_ENABLED=0 go test ./...`
Expected: all `ok`.

- [ ] **Step 5: Measure the win**

```bash
go build -o /tmp/par2bench-cgo ./cmd/par2bench/
cd /path/to/pristine_scaled && /tmp/par2bench-cgo -op verify -par scaled.par2
```
Expected: `peak_rss_bytes` well under 200 MB, versus ~1.3 GB before. Record the number.

- [ ] **Step 6: Commit**

```bash
gofmt -w par2/decoder.go par2/streaming_scan_test.go
git add par2/decoder.go par2/streaming_scan_test.go
git commit -m "perf: scan data files through a sliding window"
```

---

### Task 4: Streaming fold in rsec16 (cgo backend)

**Files:**
- Create: `rsec16/fold.go`
- Create: `rsec16/fold_test.go`
- Modify: `rsec16/apply_gf16.go`

**Interfaces:**
- Consumes: `gf16.Context` (`NewBuffer`, `Prepare`, `MulAdd`, `Finish`, `BufSize`, `Stride`).
- Produces:
  ```go
  // FoldInputs computes out[i] = sum over j of m.At(i, j) * input j, pulling
  // each input through next one at a time so the caller never holds them all.
  // Every out row is fully overwritten and must be sliceSize long.
  func FoldInputs(m gf2p16.Matrix, numInputs, sliceSize int, next func(j int, buf []byte) error, out [][]byte, numGoroutines int) error
  ```

- [ ] **Step 1: Write the failing test**

Create `rsec16/fold_test.go`:

```go
package rsec16

import (
	"math/rand"
	"testing"

	"github.com/javi11/gopar-turbo/gf2p16"
	"github.com/stretchr/testify/require"
)

// FoldInputs must agree exactly with the all-in-memory matrix application.
func TestFoldInputsMatchesApplyMatrix(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	for _, tc := range []struct{ numInputs, numOut, sliceSize int }{
		{4, 2, 64},
		{9, 3, 256},
		{17, 5, 1024},
	} {
		in := make([][]byte, tc.numInputs)
		for i := range in {
			in[i] = make([]byte, tc.sliceSize)
			rng.Read(in[i])
		}
		m := gf2p16.NewMatrixFromFunction(tc.numOut, tc.numInputs, func(i, j int) gf2p16.T {
			return gf2p16.T(rng.Intn(65536))
		})

		want := make([][]byte, tc.numOut)
		for i := range want {
			want[i] = make([]byte, tc.sliceSize)
		}
		c := Coder{numGoroutines: 4}
		c.applyMatrix(m, in, want)

		got := make([][]byte, tc.numOut)
		for i := range got {
			got[i] = make([]byte, tc.sliceSize)
		}
		err := FoldInputs(m, tc.numInputs, tc.sliceSize, func(j int, buf []byte) error {
			copy(buf, in[j])
			return nil
		}, got, 4)
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
}

func TestFoldInputsPropagatesReadError(t *testing.T) {
	m := gf2p16.NewMatrixFromFunction(1, 2, func(i, j int) gf2p16.T { return 1 })
	out := [][]byte{make([]byte, 64)}
	err := FoldInputs(m, 2, 64, func(j int, buf []byte) error {
		if j == 1 {
			return errShardRead
		}
		return nil
	}, out, 1)
	require.ErrorIs(t, err, errShardRead)
}

var errShardRead = errTest("read failed")

type errTest string

func (e errTest) Error() string { return string(e) }
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./rsec16/ -run TestFoldInputs -v`
Expected: FAIL to compile — `undefined: FoldInputs`.

- [ ] **Step 3: Write the implementation**

Create `rsec16/fold.go`:

```go
package rsec16

import (
	"github.com/javi11/gopar-turbo/gf16"
	"github.com/javi11/gopar-turbo/gf2p16"
)

// FoldInputs computes out[i] = sum over j of m.At(i, j) * input j.
//
// Inputs are pulled one at a time through next, which must fill the supplied
// buffer with input j. The buffer is reused between calls, so next must not
// retain it. Only the output accumulators stay resident, which makes peak
// memory proportional to len(out) rather than to numInputs.
//
// Every row of out is fully overwritten and must be sliceSize bytes long.
func FoldInputs(m gf2p16.Matrix, numInputs, sliceSize int, next func(j int, buf []byte) error, out [][]byte, numGoroutines int) error {
	if gf16.Accelerated() {
		return foldInputsGF16(m, numInputs, sliceSize, next, out, numGoroutines)
	}
	return foldInputsPureGo(m, numInputs, sliceSize, next, out, numGoroutines)
}

func foldInputsGF16(m gf2p16.Matrix, numInputs, sliceSize int, next func(j int, buf []byte) error, out [][]byte, numGoroutines int) error {
	ctx, err := gf16.NewContext(sliceSize)
	if err != nil {
		return err
	}
	defer ctx.Close()

	accs := make([][]byte, len(out))
	zero := make([]byte, sliceSize)
	for i := range out {
		accs[i] = ctx.NewBuffer()
		ctx.Prepare(accs[i], zero)
	}

	raw := make([]byte, sliceSize)
	prepared := ctx.NewBuffer()
	for j := 0; j < numInputs; j++ {
		for k := range raw {
			raw[k] = 0
		}
		if err := next(j, raw); err != nil {
			return err
		}
		ctx.Prepare(prepared, raw)
		for i := range out {
			ctx.MulAdd(accs[i], prepared, uint16(m.At(i, j)))
		}
	}

	for i := range out {
		ctx.Finish(accs[i], out[i])
	}
	return nil
}
```

Note: parallelism across output rows is deliberately omitted here — the fold is
bounded by input I/O, and a correct sequential version comes first. Task 6
revisits parallelism only if the benchmark shows it matters.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./rsec16/ -run TestFoldInputs -v`
Expected: PASS (both subtests).

- [ ] **Step 5: Commit**

```bash
gofmt -w rsec16/fold.go rsec16/fold_test.go
git add rsec16/fold.go rsec16/fold_test.go
git commit -m "feat: add streaming FoldInputs to rsec16"
```

---

### Task 5: Pure-Go fold path

**Files:**
- Modify: `rsec16/fold.go`
- Test: `rsec16/fold_test.go` (already covers both via `FoldInputs` dispatch)

**Interfaces:**
- Consumes: `gf2p16.Matrix`, `gf2p16` mul/add helpers used by `applyMatrixParallelData`.
- Produces: `foldInputsPureGo` with the same signature as `foldInputsGF16`.

- [ ] **Step 1: Run the existing test under the pure-Go backend to see it fail**

Run: `CGO_ENABLED=0 go test ./rsec16/ -run TestFoldInputs -v`
Expected: FAIL to compile — `undefined: foldInputsPureGo`.

- [ ] **Step 2: Write the implementation**

Append to `rsec16/fold.go`:

```go
// foldInputsPureGo mirrors foldInputsGF16 without the SIMD backend. It
// accumulates directly into out, which the gf2p16 helpers can do in place.
func foldInputsPureGo(m gf2p16.Matrix, numInputs, sliceSize int, next func(j int, buf []byte) error, out [][]byte, numGoroutines int) error {
	for i := range out {
		for k := range out[i] {
			out[i][k] = 0
		}
	}

	raw := make([]byte, sliceSize)
	for j := 0; j < numInputs; j++ {
		for k := range raw {
			raw[k] = 0
		}
		if err := next(j, raw); err != nil {
			return err
		}
		for i := range out {
			// out[i] ^= m[i][j] * raw. Starting from a zeroed out row,
			// accumulating every input is equivalent to the
			// MulByteSliceLE-then-MulAndAddByteSliceLE sequence in
			// rsec16/matrix.go applyMatrixSlice.
			gf2p16.MulAndAddByteSliceLE(m.At(i, j), raw, out[i])
		}
	}
	return nil
}
```

`MulByteSliceLE(c T, in, out []byte)` and `MulAndAddByteSliceLE(c T, in, out []byte)`
are declared for both amd64 (`gf2p16/slice_amd64.go`) and every other
architecture (`gf2p16/slice_nonamd64.go`), so this compiles everywhere.

- [ ] **Step 3: Run tests on both backends**

Run: `CGO_ENABLED=0 go test ./rsec16/ -run TestFoldInputs -v`
Expected: PASS.

Run: `go test ./rsec16/ -run TestFoldInputs -v`
Expected: PASS.

- [ ] **Step 4: Add a cross-backend equivalence check**

Append to `rsec16/fold_test.go`:

```go
// Golden vector: both backends must produce these exact bytes, so a build
// with cgo and one without stay interchangeable.
func TestFoldInputsGoldenVector(t *testing.T) {
	sliceSize := 64
	in := make([][]byte, 3)
	for i := range in {
		in[i] = make([]byte, sliceSize)
		for k := range in[i] {
			in[i][k] = byte(i*7 + k*3)
		}
	}
	m := gf2p16.NewMatrixFromFunction(2, 3, func(i, j int) gf2p16.T {
		return gf2p16.T(1 + i*3 + j)
	})
	out := make([][]byte, 2)
	for i := range out {
		out[i] = make([]byte, sliceSize)
	}
	require.NoError(t, FoldInputs(m, 3, sliceSize, func(j int, buf []byte) error {
		copy(buf, in[j])
		return nil
	}, out, 2))

	want := make([][]byte, 2)
	for i := range want {
		want[i] = make([]byte, sliceSize)
	}
	c := Coder{numGoroutines: 2}
	c.applyMatrix(m, in, want)
	require.Equal(t, want, out)
}
```

Run: `go test ./rsec16/ -run TestFoldInputsGolden && CGO_ENABLED=0 go test ./rsec16/ -run TestFoldInputsGolden`
Expected: PASS both.

- [ ] **Step 5: Commit**

```bash
gofmt -w rsec16/fold.go rsec16/fold_test.go
git add rsec16/fold.go rsec16/fold_test.go
git commit -m "feat: add pure-Go streaming fold path"
```

---

### Task 6: Repair reconstructs by streaming fold

**Files:**
- Modify: `par2/decoder.go` (`newCoderAndShards` ~line 613, the repair body ~line 660-775)
- Modify: `par2/repair.go`, `par2/verify.go` (add `MemoryBudget`)
- Test: `par2/streaming_repair_test.go` (create)

**Interfaces:**
- Consumes: `rsec16.FoldInputs` (Tasks 4-5), `Decoder.readShard` (Task 2), `shardIntegrityInfo.present/foundAt` (Task 2).
- Produces: repair that never holds the full set; `RepairOptions.MemoryBudget int`.

- [ ] **Step 1: Write the failing test**

Create `par2/streaming_repair_test.go`:

```go
package par2

import (
	"path/filepath"
	"testing"

	"github.com/javi11/gopar-turbo/memfs"
	"github.com/stretchr/testify/require"
)

// Repair must reconstruct byte-identical content while streaming.
func TestStreamingRepairRestoresExactBytes(t *testing.T) {
	workingDir := memfs.RootDir()
	fs := makeShiftTestMemFS(workingDir)
	buildPAR2Data(t, fs, workingDir, 4, 2)

	want := makeShiftTestMemFS(workingDir)
	wantData, err := want.ReadFile(filepath.Join(workingDir, "big.rar"))
	require.NoError(t, err)

	perturbFile(t, fs, "big.rar")

	result, err := repair(testFileIO{t, fs}, filepath.Join(workingDir, "file.par2"), RepairOptions{})
	require.NoError(t, err)
	require.Len(t, result.RepairedPaths, 1)

	got, err := fs.ReadFile(filepath.Join(workingDir, "big.rar"))
	require.NoError(t, err)
	require.Equal(t, wantData, got)
}

// A budget small enough to force several chunk passes must not change output.
func TestStreamingRepairChunkedMatchesUnchunked(t *testing.T) {
	workingDir := memfs.RootDir()

	run := func(budget int) []byte {
		fs := makeShiftTestMemFS(workingDir)
		buildPAR2Data(t, fs, workingDir, 4, 2)
		perturbFile(t, fs, "big.rar")
		_, err := repair(testFileIO{t, fs}, filepath.Join(workingDir, "file.par2"),
			RepairOptions{MemoryBudget: budget})
		require.NoError(t, err)
		data, err := fs.ReadFile(filepath.Join(workingDir, "big.rar"))
		require.NoError(t, err)
		return data
	}

	require.Equal(t, run(0), run(8), "a tiny budget must not change the result")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./par2/ -run TestStreamingRepair -v`
Expected: `TestStreamingRepairChunkedMatchesUnchunked` fails to compile — `unknown field MemoryBudget`.

- [ ] **Step 3: Add the option and wire it**

In `par2/repair.go` and `par2/verify.go`, add to the options struct:

```go
	// MemoryBudget caps the bytes held for reconstruction accumulators.
	// Zero selects half of physical memory, matching par2cmdline's -m.
	// Repair splits slices into byte-range chunks and makes multiple
	// passes over the inputs when the budget requires it.
	MemoryBudget int
```

Pass it into the decoder next to `scanPolicy`, adding a `memoryBudget int` field to `Decoder`.

- [ ] **Step 4: Replace the reconstruction**

Replace `newCoderAndShards` and the reconstruct call in the repair body with a streaming version. Build the row lists from presence, then fold:

```go
// reconstructMissing fills in the missing data shards by folding every
// available input into accumulators for the missing shards only.
func (d *Decoder) reconstructMissing() (map[int][]byte, error) {
	var availableRows, missingRows []int
	shardIndex := 0
	type src struct {
		info    shardIntegrityInfo
		isParity bool
		parity  int
	}
	var inputs []src

	for _, info := range d.fileIntegrityInfos {
		for _, si := range info.shardInfos {
			if si.present {
				availableRows = append(availableRows, shardIndex)
				inputs = append(inputs, src{info: si})
			} else {
				missingRows = append(missingRows, shardIndex)
			}
			shardIndex++
		}
	}
	if len(missingRows) == 0 {
		return nil, nil
	}

	var usedParityRows []int
	for i := 0; i < len(d.parityShards) && len(inputs) < shardIndex; i++ {
		if d.parityShards[i] != nil {
			usedParityRows = append(usedParityRows, i)
			inputs = append(inputs, src{isParity: true, parity: i})
		}
	}
	if len(inputs) < shardIndex {
		return nil, rsec16.NotEnoughParityShardsError{}
	}

	coder, err := rsec16.NewCoderPAR2Vandermonde(shardIndex, len(d.parityShards), d.numGoroutines)
	if err != nil {
		return nil, err
	}
	m, err := coder.ReconstructionMatrix(availableRows, missingRows, usedParityRows)
	if err != nil {
		return nil, err
	}

	out := make([][]byte, len(missingRows))
	for i := range out {
		out[i] = make([]byte, d.sliceByteCount)
	}

	err = rsec16.FoldInputs(m, len(inputs), d.sliceByteCount, func(j int, buf []byte) error {
		if inputs[j].isParity {
			copy(buf, d.parityShards[inputs[j].parity])
			return nil
		}
		return d.readShard(inputs[j].info, buf)
	}, out, d.numGoroutines)
	if err != nil {
		return nil, err
	}

	result := make(map[int][]byte, len(missingRows))
	for i, row := range missingRows {
		result[row] = out[i]
	}
	return result, nil
}
```

`makeReconstructionMatrix` is currently unexported in `rsec16`. Export a thin
wrapper on `Coder` so the decoder can build the matrix without duplicating the
math, in `rsec16/coder.go`:

```go
// ReconstructionMatrix returns the matrix that maps the available data rows
// followed by the used parity rows onto the missing data rows.
func (c Coder) ReconstructionMatrix(availableRows, missingRows, usedParityRows []int) (gf2p16.Matrix, error) {
	return makeReconstructionMatrix(c.dataShards, availableRows, missingRows, usedParityRows, c.parityMatrix)
}
```

In the repair write loop, use the reconstructed map for shards that were absent
and `readShard` for shards that were present, so no full-set buffer is built.

- [ ] **Step 5: Apply the memory budget**

Wrap the fold in a chunk loop. Chunk size must be even and a multiple of the
gf16 stride; clamp to the slice size:

```go
// chunkSizeFor returns the byte-range chunk to reconstruct per pass so that
// len(missing) accumulators fit inside the budget.
func chunkSizeFor(budget, sliceByteCount, missing, stride int) int {
	if budget <= 0 {
		budget = defaultMemoryBudget()
	}
	per := budget / missing
	if per >= sliceByteCount {
		return sliceByteCount
	}
	per -= per % stride
	if per < stride {
		per = stride
	}
	return per
}
```

`defaultMemoryBudget()` returns half of physical memory. Obtain it from
`sysctl hw.memsize` on darwin and `/proc/meminfo` on linux, behind build tags,
falling back to 1 GiB when it cannot be determined.

For each chunk, call `FoldInputs` with `sliceSize = chunkLen` and a `next` that
reads only that byte range of each input, then append the chunk to the output
shard.

- [ ] **Step 6: Run the full suite on both backends**

Run: `go test -race ./... && CGO_ENABLED=0 go test ./...`
Expected: all `ok`, including `TestRealTool*` and `TestRepairMisalignedDataNeedsSearchEnabled`.

- [ ] **Step 7: Commit**

```bash
gofmt -w par2/decoder.go par2/repair.go par2/verify.go par2/streaming_repair_test.go rsec16/coder.go
git add par2 rsec16
git commit -m "perf: reconstruct by streaming fold within a memory budget"
```

---

### Task 7: Verify the win end to end

**Files:**
- Modify: `README.md` (document `MemoryBudget`)
- Modify: `bench/README.md` (note the memory expectation)

**Interfaces:**
- Consumes: everything above.
- Produces: measured numbers for the report.

- [ ] **Step 1: Rebuild the benchmark binaries**

```bash
SP=<scratchpad>
go build -o "$SP/bin/par2bench-cgo" ./cmd/par2bench/
CGO_ENABLED=0 go build -o "$SP/bin/par2bench-pure" ./cmd/par2bench/
```

- [ ] **Step 2: Run the scaled sweep**

```bash
python3 bench/run.py --dataset scaled-1GB \
  --pristine "$SP/bench/pristine_scaled" --index scaled.par2 \
  --work "$SP/bench/work" --results "$SP/bench/results" \
  --gopar-cgo "$SP/bin/par2bench-cgo" --gopar-pure "$SP/bin/par2bench-pure" \
  --par2-turbo "$SP/par2cmdline-turbo/par2" \
  --slice-size 2380956 --reps 3 --missing-files 1 --corrupt-slices 50
```
Expected: every repair row reports `md5=ok`; `peak_rss_bytes` for gopar rows drops from ~1.3-3.0 GB to a few hundred MB or less.

- [ ] **Step 3: Run the full-set sweep**

```bash
python3 bench/run.py --dataset full-4.36GiB \
  --pristine "$SP/bench/pristine_full" \
  --index 3b90183365614f85aec3b6f804331f07.par2 \
  --work "$SP/bench/work" --results "$SP/bench/results" \
  --gopar-cgo "$SP/bin/par2bench-cgo" --gopar-pure "$SP/bin/par2bench-pure" \
  --par2-turbo "$SP/par2cmdline-turbo/par2" \
  --slice-size 2380956 --reps 1 --missing-files 5 --corrupt-slices 200
```
Expected: completes without swapping; gopar peak RSS far below the 13.7 GB projected for the old code; all repairs `md5=ok`.

- [ ] **Step 4: Document the option**

Add a short `MemoryBudget` subsection to `README.md` next to the misaligned-data
section, stating the default (half of physical memory) and that repair makes
extra passes over the inputs when the budget is tight.

- [ ] **Step 5: Commit**

```bash
gofmt -l . | grep -v crc32_test.go   # expect empty
git add README.md bench/README.md
git commit -m "docs: document the repair memory budget"
```

---

## Self-Review

**Spec coverage.** Streaming scan → Tasks 1-3. Presence-not-payload → Task 2.
Streaming fold → Tasks 4-5. Memory budget and chunked passes → Task 6.
Equivalence, budget, backend, and regression tests → Tasks 4-6. End-to-end
benchmark → Task 7. `fileIO` streaming read → Task 1. Every spec section maps
to a task.

**Resolved before execution:** the pure-Go fold uses
`gf2p16.MulAndAddByteSliceLE(c T, in, out []byte)`, which exists for amd64 and
non-amd64 alike. `defaultMemoryBudget()` reads `hw.memsize` via sysctl on
darwin and `MemTotal` from `/proc/meminfo` on linux, behind build tags, with a
1 GiB fallback on any other platform or on error.

**Type consistency.** `readerAtCloser` (Task 1) is used by `readShard` (Task 2)
and `fillFileIntegrityInfos` (Task 3). `shardIntegrityInfo.present/foundAt`
(Task 2) is read by Tasks 3 and 6. `FoldInputs` (Task 4) keeps one signature
across Tasks 5 and 6. `ReconstructionMatrix` is added in Task 6 where it is
first used.
