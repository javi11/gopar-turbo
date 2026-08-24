# Performance Parity Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Bring gopar-turbo's verify and repair wall-clock to par2cmdline-turbo's level on the benchmark set (cgo backend), without regressing memory or changing output bytes.

**Architecture:** Three independent workstreams: (1) the streaming fold becomes parallel, batched (`MulAddMulti`), and pipelined (double-buffered prefetch), reusing `applyMatrixGF16`'s proven per-worker-context pattern; (2) the file scan runs on a worker pool with one mutex around match recording; (3) recovery volumes are parsed packet-by-packet through a reusable buffer instead of `ReadFile`, and parity load overlaps the scan.

**Tech Stack:** Go 1.26+, cgo ParPar kernels (`gf16`), pure-Go `gf2p16` fallback, `testify/require`, `memfs`.

## Global Constraints

- Output must be **bit-identical** on both backends; `Coder.applyMatrix` / `applyMatrixParallelData` remain the equivalence oracles.
- Every task passes `go test -race ./...` and `CGO_ENABLED=0 go test ./...`.
- Public API additive only; **no new options are expected in this plan**.
- `gf16/vendor` untouched. No new module dependencies (stdlib only).
- Never hard-code slice sizes; the benchmark set's is `2380956`.
- Throughput gate: the parallel fold must reach the aggregate of `BenchmarkReconstruct_1000x50_64K`'s matrix path (~97 GB/s on this host) in Task 1's benchmark **before** Tasks 4+ (par2 integration) land.
- Peak RSS must not regress from bench/RESULTS.md numbers.
- `gofmt -w` every touched file. Pre-existing unformatted files (`par2/crc32_test.go`, `gf2p16/slice*.go`, `gf2p16/t_nonamd64.go`) stay untouched.
- Benchmark binaries build from `cmd/par2bench`; scratchpad path for artifacts: use the session scratchpad, not `/tmp`.

---

### Task 1: Fold throughput benchmark (the gate)

**Files:**
- Create: `rsec16/fold_bench_test.go`

**Interfaces:**
- Consumes: `FoldInputs` (existing signature: `FoldInputs(m gf2p16.Matrix, numInputs, sliceSize int, next func(j int, buf []byte) error, out [][]byte, numGoroutines int) error`).
- Produces: `BenchmarkFoldInputs_Realistic` and `BenchmarkFoldInputs_ApplyMatrixReference` — the numbers Tasks 2–3 tune against.

- [ ] **Step 1: Write the benchmark**

```go
package rsec16

import (
	"math/rand"
	"runtime"
	"testing"

	"github.com/javi11/gopar-turbo/gf2p16"
)

// benchFoldShape mirrors the benchmark set's repair-missing workload,
// scaled down in input count to keep memory sane: sliceSize matches the
// real set, and outputs/inputs keep the same ratio (~210/1965).
const (
	benchFoldInputs    = 256
	benchFoldOutputs   = 28
	benchFoldSliceSize = 2380956
)

func benchFoldSetup(b *testing.B) ([][]byte, gf2p16.Matrix, [][]byte) {
	b.Helper()
	rng := rand.New(rand.NewSource(42))
	in := make([][]byte, benchFoldInputs)
	for i := range in {
		in[i] = make([]byte, benchFoldSliceSize)
		rng.Read(in[i])
	}
	m := gf2p16.NewMatrixFromFunction(benchFoldOutputs, benchFoldInputs,
		func(i, j int) gf2p16.T { return gf2p16.T(rng.Intn(65535) + 1) })
	out := make([][]byte, benchFoldOutputs)
	for i := range out {
		out[i] = make([]byte, benchFoldSliceSize)
	}
	return in, m, out
}

// SetBytes reports *GF work* (inputs x sliceSize x outputs per op), so the
// reported MB/s is directly comparable to the ~97 GB/s aggregate that
// BenchmarkReconstruct_1000x50_64K proves the matrix path can do.
func BenchmarkFoldInputs_Realistic(b *testing.B) {
	in, m, out := benchFoldSetup(b)
	b.SetBytes(int64(benchFoldInputs) * benchFoldSliceSize * int64(benchFoldOutputs))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		err := FoldInputs(m, benchFoldInputs, benchFoldSliceSize, func(j int, buf []byte) error {
			copy(buf, in[j])
			return nil
		}, out, runtime.NumCPU())
		if err != nil {
			b.Fatal(err)
		}
	}
}

// The reference: the existing all-in-memory parallel path on the same shape.
func BenchmarkFoldInputs_ApplyMatrixReference(b *testing.B) {
	in, m, out := benchFoldSetup(b)
	c := Coder{numGoroutines: runtime.NumCPU()}
	b.SetBytes(int64(benchFoldInputs) * benchFoldSliceSize * int64(benchFoldOutputs))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.applyMatrix(m, in, out)
	}
}
```

- [ ] **Step 2: Run it to record the baseline**

Run: `go test ./rsec16/ -bench 'BenchmarkFoldInputs' -benchtime 3x -run xxx`
Expected: compiles and runs. Record both numbers in the commit message.
`_Realistic` will be roughly 10× slower than `_ApplyMatrixReference` — that
gap is what Task 2 closes. The reference number (per-op bytes/s) is the gate.

- [ ] **Step 3: Commit**

```bash
gofmt -w rsec16/fold_bench_test.go
git add rsec16/fold_bench_test.go
git commit -m "test: benchmark FoldInputs against the parallel matrix path

Baseline on Apple M4: <paste both numbers>"
```

---

### Task 2: Parallel, batched, pipelined fold (cgo)

**Files:**
- Modify: `rsec16/fold.go` (replace `foldInputsGF16`)
- Test: `rsec16/fold_test.go` (extend)

**Interfaces:**
- Consumes: `gf16.Context` (`NewContext`, `NewBuffer`, `Prepare`, `MulAddMulti(dst, srcs, offset, length, coeffs)`, `Finish`, `BufSize`, `Stride`), `calculateParallelParams(totalLength, numGoroutines, minPerGoroutineLength, perGoroutineLengthDivisor int) (perGoroutineLength, newNumGoroutines int)` from `rsec16/matrix.go`.
- Produces: `FoldInputs` with the **same signature**, now honoring `numGoroutines`. `next` contract addition (document in the `FoldInputs` doc comment): calls may run ahead of folding (prefetch) but remain strictly sequential, `j = 0..numInputs-1`, one at a time from a single goroutine.

- [ ] **Step 1: Write the failing tests**

Append to `rsec16/fold_test.go`:

```go
// The parallel fold must agree with the matrix path at awkward shapes:
// batch boundaries, more workers than rows, single input, tiny slices.
func TestFoldInputsParallelMatchesApplyMatrix(t *testing.T) {
	rng := rand.New(rand.NewSource(31))
	for _, tc := range []struct {
		name                          string
		numInputs, numOut, sliceSize  int
		numGoroutines                 int
	}{
		{"one batch exactly", 16, 3, 4096, 4},
		{"batch remainder", 37, 5, 4096, 8},
		{"fewer inputs than batch", 3, 2, 4096, 4},
		{"more workers than rows", 20, 2, 4096, 16},
		{"single input single row", 1, 1, 64, 4},
		{"goroutines=1", 37, 5, 4096, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := make([][]byte, tc.numInputs)
			for i := range in {
				in[i] = make([]byte, tc.sliceSize)
				rng.Read(in[i])
			}
			m := gf2p16.NewMatrixFromFunction(tc.numOut, tc.numInputs,
				func(i, j int) gf2p16.T { return gf2p16.T(rng.Intn(65536)) })

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
			}, got, tc.numGoroutines)
			require.NoError(t, err)
			require.Equal(t, want, got)
		})
	}
}

// next must be called exactly once per input, strictly in order, even with
// prefetch running ahead of the fold.
func TestFoldInputsCallsNextSequentially(t *testing.T) {
	numInputs, sliceSize := 40, 4096
	m := gf2p16.NewMatrixFromFunction(2, numInputs,
		func(i, j int) gf2p16.T { return gf2p16.T(i + j + 1) })
	out := [][]byte{make([]byte, sliceSize), make([]byte, sliceSize)}

	var calls []int
	err := FoldInputs(m, numInputs, sliceSize, func(j int, buf []byte) error {
		calls = append(calls, j) // single-goroutine contract: no lock needed
		return nil
	}, out, 8)
	require.NoError(t, err)
	require.Len(t, calls, numInputs)
	for j, got := range calls {
		require.Equal(t, j, got, "next call order")
	}
}

// An error from next mid-batch must abort promptly and be returned.
func TestFoldInputsPropagatesMidBatchError(t *testing.T) {
	m := gf2p16.NewMatrixFromFunction(2, 40, func(i, j int) gf2p16.T { return 1 })
	out := [][]byte{make([]byte, 4096), make([]byte, 4096)}
	err := FoldInputs(m, 40, 4096, func(j int, buf []byte) error {
		if j == 21 { // second batch, mid-batch
			return errShardRead
		}
		return nil
	}, out, 8)
	require.ErrorIs(t, err, errShardRead)
}
```

- [ ] **Step 2: Run to verify the order test fails or the equivalence tests pass vacuously**

Run: `go test ./rsec16/ -run 'TestFoldInputsParallel|TestFoldInputsCallsNext|TestFoldInputsPropagatesMidBatch' -v`
Expected: PASS with the current sequential implementation (it already
satisfies all three). These are **regression guards** for the rewrite —
record that they pass, then proceed. The failing check is the benchmark gate
in Step 4.

- [ ] **Step 3: Replace `foldInputsGF16` with the parallel pipeline**

In `rsec16/fold.go`:

```go
// foldBatchSize is how many prepared inputs are folded per accumulator pass.
// Larger batches cut accumulator cache traffic (one MulAddMulti pass per
// batch instead of per input) at the cost of 2*B prepared buffers.
const foldBatchSize = 16

type foldUnit struct {
	row     int
	offset  int
	length  int
	srcs    [][]byte
	coeffs  []uint16
}

func foldInputsGF16(m gf2p16.Matrix, numInputs, sliceSize int, next func(j int, buf []byte) error, out [][]byte, numGoroutines int) error {
	ctx, err := gf16.NewContext(sliceSize)
	if err != nil {
		return err
	}
	defer ctx.Close()

	if numGoroutines < 1 {
		numGoroutines = 1
	}
	bufSize, stride := ctx.BufSize(), ctx.Stride()

	// Accumulators stay in prepared layout for the whole fold.
	zero := make([]byte, sliceSize)
	accs := make([][]byte, len(out))
	for i := range out {
		accs[i] = ctx.NewBuffer()
		ctx.Prepare(accs[i], zero)
	}

	B := foldBatchSize
	if B > numInputs {
		B = numInputs
	}

	// Two batch banks: the reader prepares bank k^1 while workers fold bank k.
	var banks [2][][]byte
	for k := range banks {
		banks[k] = make([][]byte, B)
		for b := range banks[k] {
			banks[k][b] = ctx.NewBuffer()
		}
	}

	// Range split of the accumulators; rows x ranges are the work units.
	perRange, numRanges := calculateParallelParams(
		bufSize, (numGoroutines+len(out)-1)/len(out), stride, stride)

	// Long-lived workers, each with its own context (Mul* calls share
	// mutScratch, so contexts cannot be shared across concurrent calls).
	units := make(chan foldUnit)
	workerErr := make(chan error, numGoroutines)
	var workers sync.WaitGroup
	workers.Add(numGoroutines)
	for w := 0; w < numGoroutines; w++ {
		go func() {
			defer workers.Done()
			wctx, err := gf16.NewContext(sliceSize)
			if err != nil {
				workerErr <- err
				// Drain so the dispatcher never blocks.
				for range units {
				}
				return
			}
			defer wctx.Close()
			for u := range units {
				wctx.MulAddMulti(accs[u.row], u.srcs, u.offset, u.length, u.coeffs)
			}
		}()
	}

	// The reader prepares one bank ahead. readBank returns how many inputs
	// it placed into the bank (0 at EOF) or the first error from next.
	raw := make([]byte, sliceSize)
	nextJ := 0
	readBank := func(bank [][]byte) (int, [][]uint16, error) {
		n := 0
		for ; n < B && nextJ < numInputs; n++ {
			clear(raw)
			if err := next(nextJ, raw); err != nil {
				return 0, nil, err
			}
			ctx.Prepare(bank[n], raw)
			nextJ++
		}
		if n == 0 {
			return 0, nil, nil
		}
		// coeffs[i] is row i's coefficients for this batch.
		base := nextJ - n
		coeffs := make([][]uint16, len(out))
		for i := range out {
			coeffs[i] = make([]uint16, n)
			for b := 0; b < n; b++ {
				coeffs[i][b] = uint16(m.At(i, base+b))
			}
		}
		return n, coeffs, nil
	}

	type prepared struct {
		n      int
		coeffs [][]uint16
		err    error
	}
	ready := make(chan prepared, 1)
	// Kick off the first read synchronously so errors surface immediately.
	dispatch := func(bank [][]byte, n int, coeffs [][]uint16) {
		var done sync.WaitGroup
		for i := range out {
			for r := 0; r < numRanges; r++ {
				offset := r * perRange
				length := perRange
				if offset+length > bufSize {
					length = bufSize - offset
				}
				if length <= 0 {
					continue
				}
				units <- foldUnit{row: i, offset: offset, length: length,
					srcs: bank[:n], coeffs: coeffs[i]}
			}
		}
		_ = done
	}

	var foldErr error
	k := 0
	n, coeffs, err := readBank(banks[k])
	if err != nil {
		foldErr = err
	}
	for foldErr == nil && n > 0 {
		// Prefetch the other bank while workers fold this one.
		go func(bank [][]byte) {
			pn, pc, perr := readBank(bank)
			ready <- prepared{pn, pc, perr}
		}(banks[k^1])

		dispatch(banks[k], n, coeffs)
		// Barrier: all units of this batch must land before the next batch
		// may touch the accumulators. Close-and-reopen is the simplest
		// correct barrier for a shared channel.
		close(units)
		workers.Wait()
		select {
		case err := <-workerErr:
			foldErr = err
		default:
		}
		units = make(chan foldUnit)
		workers.Add(numGoroutines)
		for w := 0; w < numGoroutines; w++ {
			go func() {
				defer workers.Done()
				wctx, err := gf16.NewContext(sliceSize)
				if err != nil {
					workerErr <- err
					for range units {
					}
					return
				}
				defer wctx.Close()
				for u := range units {
					wctx.MulAddMulti(accs[u.row], u.srcs, u.offset, u.length, u.coeffs)
				}
			}()
		}

		p := <-ready
		if p.err != nil {
			foldErr = p.err
		}
		n, coeffs = p.n, p.coeffs
		k ^= 1
	}
	close(units)
	workers.Wait()
	if foldErr != nil {
		return foldErr
	}
	select {
	case err := <-workerErr:
		return err
	default:
	}

	// Untransform accumulators, in parallel (Finish is stateless).
	var wg sync.WaitGroup
	wg.Add(len(out))
	for i := range out {
		go func(i int) {
			defer wg.Done()
			ctx.Finish(accs[i], out[i])
		}(i)
	}
	wg.Wait()
	return nil
}
```

**Implementation note (not optional): the close-and-respawn barrier above is
the reference shape, but respawning workers (and their contexts) per batch is
wasteful — ~123 batches on the full set means ~1230 context creates.** During
implementation, hoist worker+context lifetime out of the batch loop: keep
workers alive on a persistent `units` channel and use a `sync.WaitGroup`
counting *units issued per batch* (`unitsWG.Add(1)` per send, `u.done()` in
the worker after `MulAddMulti`, `unitsWG.Wait()` as the barrier). The plan
shows the simpler shape for clarity; the committed version must use the
persistent-worker form and keep all tests green. Add `sync` to imports.

- [ ] **Step 4: Verify correctness, race, and the throughput gate**

Run: `go test ./rsec16/ -run TestFoldInputs -count=1 && go test -race ./rsec16/ -run TestFoldInputs -count=1`
Expected: all PASS.

Run: `go test ./rsec16/ -bench 'BenchmarkFoldInputs' -benchtime 5x -run xxx`
Expected: `_Realistic` within ~15% of `_ApplyMatrixReference`. If not, tune —
in order: (a) `foldBatchSize` (try 8/24/32), (b) per-unit range (`perRange`
targeting 256–512 KiB), (c) check the reader is not the bottleneck (profile
with `-cpuprofile`). Do not proceed until the gate holds.

- [ ] **Step 5: Commit**

```bash
gofmt -w rsec16/fold.go rsec16/fold_test.go
git add rsec16/fold.go rsec16/fold_test.go
git commit -m "perf: parallel, batched, pipelined gf16 fold

<paste gate benchmark numbers: before/after/reference>"
```

---

### Task 3: Parallel pure-Go fold

**Files:**
- Modify: `rsec16/fold.go` (replace `foldInputsPureGo`)
- Test: existing `TestFoldInputs*` (they dispatch through `FoldInputs`)

**Interfaces:**
- Consumes: `gf2p16.MulAndAddByteSliceLE(c gf2p16.T, in, out []byte)`.
- Produces: same-signature `foldInputsPureGo`, now honoring `numGoroutines`.

- [ ] **Step 1: Replace the implementation**

```go
// foldInputsPureGo mirrors foldInputsGF16 without the SIMD backend. Workers
// own disjoint output rows, so no locking is needed; a second raw buffer
// lets input j+1 be read while j is folded.
func foldInputsPureGo(m gf2p16.Matrix, numInputs, sliceSize int, next func(j int, buf []byte) error, out [][]byte, numGoroutines int) error {
	for i := range out {
		clear(out[i])
	}
	if numGoroutines < 1 {
		numGoroutines = 1
	}
	if numGoroutines > len(out) {
		numGoroutines = len(out)
	}

	bufs := [2][]byte{make([]byte, sliceSize), make([]byte, sliceSize)}
	type readResult struct {
		buf []byte
		err error
	}
	ready := make(chan readResult, 1)
	read := func(j int, buf []byte) {
		clear(buf)
		err := next(j, buf)
		ready <- readResult{buf, err}
	}

	go read(0, bufs[0])
	for j := 0; j < numInputs; j++ {
		r := <-ready
		if r.err != nil {
			return r.err
		}
		if j+1 < numInputs {
			go read(j+1, bufs[(j+1)%2])
		}

		var wg sync.WaitGroup
		wg.Add(numGoroutines)
		for w := 0; w < numGoroutines; w++ {
			go func(w int) {
				defer wg.Done()
				for i := w; i < len(out); i += numGoroutines {
					gf2p16.MulAndAddByteSliceLE(m.At(i, j), r.buf, out[i])
				}
			}(w)
		}
		wg.Wait()
	}
	return nil
}
```

**Bug to avoid:** the double buffer above lets `read(j+1)` write into
`bufs[(j+1)%2]` while workers fold `bufs[j%2]` — distinct buffers, safe. Do
NOT reuse a single buffer.

**Sequential-next contract caveat:** `read(j+1)` runs concurrently with the
fold of `j`, but the *next* `read` only starts after `<-ready` for the
previous one — so `next` calls never overlap and stay in order. Keep it that
way; the fold_test order test enforces it.

- [ ] **Step 2: Verify on both backends**

Run: `CGO_ENABLED=0 go test ./rsec16/ -run TestFoldInputs -count=1 -race`
Run: `go test ./rsec16/ -run TestFoldInputs -count=1`
Expected: PASS both.

Run: `CGO_ENABLED=0 go test ./rsec16/ -bench BenchmarkFoldInputs_Realistic -benchtime 3x -run xxx`
Expected: scales with cores vs the Task 1 baseline (record the number; no
hard gate for pure Go).

- [ ] **Step 3: Commit**

```bash
gofmt -w rsec16/fold.go
git add rsec16/fold.go
git commit -m "perf: parallelize the pure-Go fold across output rows"
```

---

### Task 4: Parallel scan

**Files:**
- Modify: `par2/decoder.go` (`Decoder` struct, `LoadFileData` at ~line 567, the match-record block in `scanBuffer`)
- Test: `par2/parallel_scan_test.go` (create)

**Interfaces:**
- Consumes: existing `fillFileIntegrityInfos(checksumToLocation, fileIntegrityInfos, fileIDIndices, i, info)` per-file scanner (unchanged internals).
- Produces: `LoadFileData` with identical behavior and delegate ordering for `OnDataFileLoad`; `Decoder.scanMu sync.Mutex` guarding shard-match recording and mid-scan delegate calls.

- [ ] **Step 1: Write the failing/regression tests**

Create `par2/parallel_scan_test.go`:

```go
package par2

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/javi11/gopar-turbo/memfs"
	"github.com/javi11/gopar-turbo/rsec16"
	"github.com/stretchr/testify/require"
)

// orderRecordingDelegate records OnDataFileLoad call order; other callbacks
// fall through to the do-nothing delegate. It must be safe for concurrent
// use of the *other* callbacks.
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
	_ = rsec16.DefaultNumGoroutines() // keep the import honest
}
```

- [ ] **Step 2: Run to verify current state**

Run: `go test ./par2/ -run 'TestParallelScan' -v -count=1`
Expected: PASS (sequential today trivially satisfies both). These are the
regression guards; the deliverable is that they STILL pass — under `-race` —
once the pool lands. Record, proceed.

- [ ] **Step 3: Implement the pool**

In `par2/decoder.go`, add to the `Decoder` struct (next to `memoryBudget`):

```go
	// scanMu guards shard-match recording and mid-scan delegate calls when
	// data files are scanned concurrently.
	scanMu sync.Mutex
```

Add `"sync"` to imports. In `scanBuffer`, wrap ONLY the match-recording block
(the `location := ...; for foundLocation := range foundLocations { ... }`
section) — the function needs the mutex, so thread `mu *sync.Mutex` through:
change `scanBuffer(...)` and `fillShardInfosAt(...)` to take `mu *sync.Mutex`
as their last parameter; `fillShardInfos` (the test-facing wrapper) passes
`new(sync.Mutex)`. Inside:

```go
		location := shardLocation{fileID, baseOffset + j}
		mu.Lock()
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
		mu.Unlock()
```

In `fillFileIntegrityInfos`, guard the two mid-scan delegate calls
(`OnDetectDataFileHashMismatch`, `OnDetectDataFileWrongByteCount`) with
`d.scanMu.Lock()/Unlock()` around each call, and pass `&d.scanMu` down to
`scanBuffer`.

Replace `LoadFileData`'s per-file loop (currently `for i, info := range
d.recoverySet { ... fillFileIntegrityInfos ... OnDataFileLoad ... }`) with:

```go
	type scanResult struct {
		byteCount, hits, misses int
		err                     error
	}
	results := make([]scanResult, len(d.recoverySet))

	numWorkers := d.numGoroutines
	if numWorkers < 1 {
		numWorkers = 1
	}
	if numWorkers > len(d.recoverySet) {
		numWorkers = len(d.recoverySet)
	}

	fileIdx := make(chan int)
	var wg sync.WaitGroup
	wg.Add(numWorkers)
	for w := 0; w < numWorkers; w++ {
		go func() {
			defer wg.Done()
			for i := range fileIdx {
				byteCount, hits, misses, err := d.fillFileIntegrityInfos(
					checksumToLocation, fileIntegrityInfos, fileIDIndices, i, d.recoverySet[i])
				results[i] = scanResult{byteCount, hits, misses, err}
			}
		}()
	}
	for i := range d.recoverySet {
		fileIdx <- i
	}
	close(fileIdx)
	wg.Wait()

	// Emit per-file delegate events in file order, exactly as the
	// sequential scan did, and fail on the first error in file order.
	for i, info := range d.recoverySet {
		path := d.getFilePath(info)
		res := results[i]
		d.delegate.OnDataFileLoad(i+1, len(d.recoverySet), path, res.byteCount, res.hits, res.misses, res.err)
		if res.err != nil {
			return res.err
		}
		if res.byteCount != info.byteCount {
			var startByteOffset, endByteOffset int
			if res.byteCount < info.byteCount {
				startByteOffset = res.byteCount
				endByteOffset = info.byteCount
			} else {
				startByteOffset = info.byteCount
				endByteOffset = res.byteCount
			}
			d.delegate.OnDetectCorruptDataChunk(info.fileID, path, startByteOffset, endByteOffset)
		}
	}
```

The trailing per-shard corrupt-chunk loop (already after this block) stays
sequential and unchanged. Note the simplification vs the spec: `OnDataFileLoad`
is emitted after the pool finishes rather than live-in-order — same order,
simpler code; progress-callback liveness is not a benchmarked property. Note
also: on error the sequential scan stopped early and later files never got
`OnDataFileLoad`; the pool scans all files regardless but still reports and
fails at the first erroring file in order. Document both in the commit message.

- [ ] **Step 4: Verify**

Run: `go test -race ./par2/ -count=1 && CGO_ENABLED=0 go test ./par2/ -count=1`
Expected: all PASS, including `TestParallelScan*`, `TestRealTool*`, misaligned tests.

Measure on the real set (scratchpad paths as in bench/README.md):

```bash
go build -o "$SP/bin/par2bench-cgo" ./cmd/par2bench/
cd "$SP/bench/pristine_full" && "$SP/bin/par2bench-cgo" -op verify -par 3b90183365614f85aec3b6f804331f07.par2
```
Expected: verify-intact drops from ~17s toward ~4–6s; RSS ≤ current (~890 MB).

- [ ] **Step 5: Commit**

```bash
gofmt -w par2/decoder.go par2/parallel_scan_test.go
git add par2/decoder.go par2/parallel_scan_test.go
git commit -m "perf: scan data files on a worker pool

<paste verify timing before/after>"
```

---

### Task 5: Overlap scan and parity load

**Files:**
- Modify: `par2/verify.go` (~line 75), `par2/repair.go` (~line 83)
- Test: `par2/parallel_scan_test.go` (extend)

**Interfaces:**
- Consumes: `Decoder.LoadFileData`, `Decoder.LoadParityPresence`, `Decoder.LoadParityData` — disjoint decoder fields (scan writes `fileIDIndices`/`fileIntegrityInfos`; parity writes `parityPresent`/`parityShards`; both only read `recoverySet`/`setID`/`sliceByteCount`).
- Produces: same public behavior; both phases run concurrently.

- [ ] **Step 1: Write the regression test**

Append to `par2/parallel_scan_test.go`:

```go
// Verify and repair results must be unchanged with the phases overlapped.
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
```

- [ ] **Step 2: Run (passes today — regression guard), then implement**

In `par2/verify.go`, replace the sequential

```go
	err = decoder.LoadFileData()
	if err != nil {
		return VerifyResult{}, err
	}

	err = decoder.LoadParityPresence()
	if err != nil {
		return VerifyResult{}, err
	}
```

with:

```go
	// The scan and the parity load touch disjoint files and disjoint
	// decoder fields; running them concurrently hides the shorter phase
	// entirely.
	scanErr := make(chan error, 1)
	go func() { scanErr <- decoder.LoadFileData() }()
	parityErr := decoder.LoadParityPresence()
	if err := <-scanErr; err != nil {
		return VerifyResult{}, err
	}
	if parityErr != nil {
		return VerifyResult{}, parityErr
	}
```

In `par2/repair.go`, apply the identical pattern with
`decoder.LoadParityData()` and `RepairResult{}` returns.

**Delegate caveat:** scan-side and parity-side delegate callbacks now
interleave in time. The `DecoderDelegate` interface documents no cross-phase
ordering, and `testDecoderDelegate` only logs — but implementations must be
called safely: both phases already serialize their own calls (scan via the
ordered emitter + scanMu; parity via its own loop). Add one sentence to the
`DecoderDelegate` doc comment: "Callbacks may arrive from concurrent phases;
implementations must be safe for concurrent use."

- [ ] **Step 3: Verify**

Run: `go test -race ./par2/ -count=1 && CGO_ENABLED=0 go test ./par2/ -count=1`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
gofmt -w par2/verify.go par2/repair.go par2/parallel_scan_test.go par2/decoder.go
git add par2/verify.go par2/repair.go par2/parallel_scan_test.go par2/decoder.go
git commit -m "perf: overlap the data scan with parity loading"
```

---

### Task 6: Streaming parity load

**Files:**
- Create: `par2/volume.go`
- Modify: `par2/decoder.go` (`loadParityData` at ~line 686)
- Test: `par2/volume_test.go` (create)

**Interfaces:**
- Consumes: `readNextPacket(buf *bytes.Buffer) (recoverySetID, packetType, []byte, error)` (validates magic, length, and packet hash), `readMainPacket(body []byte) (mainPacket, error)`, `readRecoveryPacket(body []byte) (exponent, recoveryPacket, error)`, `recoveryDelegate` (existing wrapper), `fileIO.OpenRead`.
- Produces:
  ```go
  // walkPackets streams the packets of one PAR2 file, calling fn with each
  // packet's set ID, type, and body. body aliases an internal reusable
  // buffer: fn must copy anything it retains.
  func walkPackets(r io.ReaderAt, size int64, fn func(setID recoverySetID, typ packetType, body []byte) error) error
  ```

- [ ] **Step 1: Write the failing tests**

Create `par2/volume_test.go`:

```go
package par2

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/javi11/gopar-turbo/memfs"
	"github.com/stretchr/testify/require"
)

// walkPackets must see exactly the packets readFile sees, in order.
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

	// Reference: count packets via the whole-buffer reader.
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

// Streaming parity load must not read volume files whole.
func TestLoadParityDoesNotReadVolumesWhole(t *testing.T) {
	workingDir := memfs.RootDir()
	fs := makeShiftTestMemFS(workingDir)
	buildPAR2Data(t, fs, workingDir, 4, 2)

	counting := newCountingFileIO(testFileIO{t, fs})
	d, err := newDecoder(counting, testDecoderDelegate{t},
		filepath.Join(workingDir, "file.par2"), 1, defaultScanPolicy(), 0)
	require.NoError(t, err)
	require.NoError(t, d.LoadFileData())
	require.NoError(t, d.LoadParityData())

	require.Zero(t, counting.wholeReads["file.vol00+01.par2"],
		"volumes must be streamed, not read whole")
	require.Equal(t, 2, d.ShardCounts().UsableParityShardCount)
	for _, shard := range d.parityShards {
		require.Len(t, shard, 4)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./par2/ -run 'TestWalkPackets|TestLoadParityDoesNotRead' -count=1`
Expected: FAIL to compile — `undefined: walkPackets`; the counting test fails
until `loadParityData` streams.

- [ ] **Step 3: Implement `walkPackets`**

Create `par2/volume.go`:

```go
package par2

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
)

// maxPacketLength caps a single packet allocation so a corrupt length field
// cannot ask for gigabytes. Recovery packets are one slice plus overhead;
// PAR2 slice sizes in the wild are far below this.
const maxPacketLength = 1 << 30

// walkPackets streams the packets of one PAR2 file, calling fn with each
// packet's set ID, type, and body. Validation (magic, length, packet hash)
// is exactly readNextPacket's. body aliases an internal reusable buffer:
// fn must copy anything it retains.
func walkPackets(r io.ReaderAt, size int64, fn func(setID recoverySetID, typ packetType, body []byte) error) error {
	headerSize := int64(sizeOfPacketHeader())
	var packetBuf []byte
	var header [64]byte

	for offset := int64(0); offset < size; {
		if size-offset < headerSize {
			return errors.New("trailing bytes too short for a packet header")
		}
		if _, err := r.ReadAt(header[:], offset); err != nil {
			return err
		}
		// Peek the length field (bytes 8..16, little-endian) to size the
		// full read; readNextPacket re-validates everything.
		length := int64(binary.LittleEndian.Uint64(header[8:16]))
		if length < headerSize || length > maxPacketLength || offset+length > size {
			return errors.New("invalid packet length")
		}
		if int64(cap(packetBuf)) < length {
			packetBuf = make([]byte, length)
		}
		packetBuf = packetBuf[:length]
		if _, err := r.ReadAt(packetBuf, offset); err != nil {
			return err
		}

		setID, typ, body, err := readNextPacket(bytes.NewBuffer(packetBuf))
		if err != nil {
			return err
		}
		if err := fn(setID, typ, body); err != nil {
			return err
		}
		offset += length
	}
	return nil
}
```

- [ ] **Step 4: Rewrite `loadParityData` to stream**

Replace the body of the per-volume closure in `loadParityData` (the
`volumeBytes, err := d.fileIO.ReadFile(match)` + `readFile(...)` block).
The new per-volume logic, preserving today's delegate calls and checks:

```go
	for i, match := range matches {
		volumeErr := func() error {
			r, size, closeFn, err := d.fileIO.OpenRead(match)
			if err != nil {
				return err
			}
			defer closeFn()

			delegate := recoveryDelegate{d.delegate}
			foundPacket := false
			var volMainPacket *mainPacket

			err = walkPackets(r, size, func(setID recoverySetID, typ packetType, body []byte) error {
				if setID != d.setID {
					delegate.OnOtherPacketSkip(setID, typ, len(body))
					return nil
				}
				foundPacket = true
				switch typ {
				case mainPacketType:
					mp, err := readMainPacket(body)
					if err != nil {
						return err
					}
					volMainPacket = &mp
					delegate.OnMainPacketLoad(mp.sliceByteCount, len(mp.recoverySet), len(mp.nonRecoverySet))
				case recoveryPacketType:
					exp, packet, err := readRecoveryPacket(body)
					if err != nil {
						return err
					}
					delegate.OnRecoveryPacketLoad(uint16(exp), len(packet.data))
					if int(exp) >= len(parityPresent) {
						grow := int(exp+1) - len(parityPresent)
						parityPresent = append(parityPresent, make([]bool, grow)...)
						parityShards = append(parityShards, make([][]byte, grow)...)
					}
					parityPresent[exp] = true
					if retain && parityShards[exp] == nil {
						shard := make([]byte, len(packet.data))
						copy(shard, packet.data)
						parityShards[exp] = shard
					}
				case creatorPacketType:
					delegate.OnCreatorPacketLoad(readCreatorPacket(body))
				default:
					delegate.OnUnknownPacketLoad(typ, len(body))
				}
				return nil
			})
			if err != nil {
				return err
			}
			if !foundPacket {
				return nil // no packets for our set: skip, like noPacketsFoundError
			}
			if volMainPacket != nil {
				if d.sliceByteCount != volMainPacket.sliceByteCount {
					return errors.New("slice byte count mismatch")
				}
				if !reflect.DeepEqual(decoderInputFileInfoIDs(d.recoverySet), volMainPacket.recoverySet) {
					return errors.New("recovery set mismatch")
				}
				if !reflect.DeepEqual(decoderInputFileInfoIDs(d.nonRecoverySet), volMainPacket.nonRecoverySet) {
					return errors.New("non-recovery set mismatch")
				}
			}
			return nil
		}()
		d.delegate.OnParityFileLoad(i+1, match, volumeErr)
		if volumeErr != nil {
			return volumeErr
		}
	}
```

**Semantics notes to preserve carefully:**
- Today `readFile` fires `OnFileDescriptionPacketLoad`/`OnIFSCPacketLoad` for
  fd/ifsc packets found in volumes; the switch above routes them to
  `OnUnknownPacketLoad` instead. Check `recoveryDelegate`'s definition in
  decoder.go — if it forwards those callbacks, add `fileDescriptionPacketType`
  and `ifscPacketType` cases that call `readFileDescriptionPacket`/
  `readIFSCPacket` purely for the delegate call and validation, retaining
  nothing. Match today's observable callbacks exactly.
- Today the main-packet checks reject a volume with a *different* main packet
  even when it carries no recovery packets for our set; `volMainPacket != nil`
  preserves that.
- "First wins" for duplicate exponents (`parityShards[exp] == nil` check)
  replaces today's "last wins". Duplicate recovery packets are byte-identical
  by packet hash, so output is unaffected; note in commit message.
- Volumes stay **sequential** here (the spec's worker pool over volumes is
  dropped: Task 5 already overlaps parity with the multi-second scan, so
  parallel volumes add complexity with nothing left to hide it behind — YAGNI).

- [ ] **Step 5: Verify, measure, commit**

Run: `go test -race ./par2/ -count=1 && CGO_ENABLED=0 go test ./par2/ -count=1`
Expected: PASS including realtool fixtures.

```bash
go build -o "$SP/bin/par2bench-cgo" ./cmd/par2bench/
cd "$SP/bench/pristine_full" && "$SP/bin/par2bench-cgo" -op verify -par 3b90183365614f85aec3b6f804331f07.par2
```
Expected: RSS drops from ~890 MB to ~100–150 MB; `repair_needed=false`.

```bash
gofmt -w par2/volume.go par2/volume_test.go par2/decoder.go
git add par2/volume.go par2/volume_test.go par2/decoder.go
git commit -m "perf: stream recovery volumes packet-by-packet

<paste verify RSS before/after>"
```

---

### Task 7: End-to-end benchmark and results update

**Files:**
- Modify: `bench/RESULTS.md`, `bench/results.csv`, `README.md` (performance section)

**Interfaces:**
- Consumes: everything above; `bench/run.py` unchanged.

- [ ] **Step 1: Rebuild and run both sweeps**

```bash
SP=<session scratchpad>
go build -o "$SP/bin/par2bench-cgo" ./cmd/par2bench/
CGO_ENABLED=0 go build -o "$SP/bin/par2bench-pure" ./cmd/par2bench/

# Archive the old CSV first (mv results.csv results_prememparallel.csv),
# then run scaled (reps 3) and full (reps 1) exactly as bench/README.md
# documents, same seeds and damage counts as the recorded runs.
```
Expected: every repair row `md5=ok`; targets from the spec:
verify-intact ≤ 4.2s, verify-damaged ≤ 6s, repair-missing ≤ 9.5s,
repair-corrupt ≤ 14s on the full set; no RSS regressions.

- [ ] **Step 2: If a target is missed**

Profile before touching anything (`-memprofile`/`-cpuprofile` flags exist on
`cmd/par2bench`; add `-cpuprofile` mirroring `-memprofile` if needed). Fix in
the owning task's file with its tests. Do not tune blind.

- [ ] **Step 3: Update the documents**

Rewrite `bench/RESULTS.md`'s tables with the new numbers (keep the old ones
in the "what changed" section as the new "before"). Refresh `bench/results.csv`.
Update README's performance section if its claims are now stale.

- [ ] **Step 4: Final verification and commit**

```bash
go test -count=1 -race ./... && CGO_ENABLED=0 go test -count=1 ./... && go vet ./...
gofmt -l . | grep -vE 'crc32_test|gf2p16/(slice|t_nonamd64)'   # expect empty
git add bench/RESULTS.md bench/results.csv README.md
git commit -m "docs: benchmark results after performance-parity work"
```

---

## Self-Review

**Spec coverage.** Fold parallel/batched/pipelined + throughput gate → Tasks
1–2. Pure-Go parallel fold → Task 3. Parallel scan with mutex + ordered
`OnDataFileLoad` + first-error-wins → Task 4. Phase overlap → Task 5.
Streaming parity (presence + retain, packet hash preserved) → Task 6.
End-to-end targets and RESULTS refresh → Task 7. Deviations from spec, both
narrowing: `OnDataFileLoad` emitted after the pool (same order, simpler) and
volumes parsed sequentially (overlap already hides them) — noted inline with
rationale.

**Placeholders.** None; every code step is concrete. Two check-and-adapt
points are explicitly scoped: `recoveryDelegate`'s fd/ifsc forwarding (Task 6
Step 4 notes) and the persistent-worker barrier form (Task 2 note) — each
states exactly what the committed version must do.

**Type consistency.** `FoldInputs` signature identical across Tasks 1–3;
`walkPackets` signature matches its test; `scanBuffer`/`fillShardInfosAt`
gain `mu *sync.Mutex` consistently and `fillShardInfos` wrapper adapts;
`newDecoder(fileIO, delegate, path, goroutines, policy, budget)` call shape
matches the current code (6 args).
