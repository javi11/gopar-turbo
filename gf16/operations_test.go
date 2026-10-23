package gf16

import (
	"bytes"
	"runtime"
	"testing"

	"github.com/javi11/gopar-turbo/gf2p16"
	"github.com/stretchr/testify/require"
)

func TestBufferSetOperations(t *testing.T) {
	for _, size := range []int{2, 130, 4096} {
		ctx, err := NewContext(size)
		require.NoError(t, err)
		defer ctx.Close()
		raw := bytes.Repeat([]byte{7, 19}, size/2)
		rows := [][]byte{ctx.NewBuffer(), ctx.NewBuffer(), ctx.NewBuffer()}
		for _, row := range rows {
			ctx.Prepare(row, raw)
		}
		set, err := NewBufferSet(ctx, rows)
		require.NoError(t, err)
		defer set.Close()
		require.NoError(t, set.Apply(ctx, []Operation{{Dst: 0, Src: 1, Coeff: 37}, {Dst: 1, Src: 1, Coeff: 19, Kind: OpScale}, {Dst: 2, Kind: OpClear}}))
		want := append([]byte(nil), raw...)
		scaled := refMul(37, raw)
		for i := range want {
			want[i] ^= scaled[i]
		}
		got := make([]byte, size)
		ctx.Finish(rows[0], got)
		require.Equal(t, want, got)
		ctx.Finish(rows[1], got)
		require.Equal(t, refMul(19, raw), got)
		ctx.Finish(rows[2], got)
		require.Equal(t, make([]byte, size), got)
		require.Error(t, set.Apply(ctx, []Operation{{Dst: 3, Src: 0, Coeff: 1}}))
		require.Error(t, set.Apply(ctx, []Operation{{Dst: 0, Src: 3, Coeff: 1}}))
		require.Error(t, set.Apply(ctx, []Operation{{Kind: 65535}}))
		set.Close()
		require.Error(t, set.Apply(ctx, nil))
	}
}

func TestBufferSetContiguousAndDependentOperations(t *testing.T) {
	const size, count = 130, 16
	ctx, err := NewContext(size)
	require.NoError(t, err)
	defer ctx.Close()
	rows := ctx.NewBuffers(count)
	want := make([][]byte, count)
	for i := range rows {
		want[i] = make([]byte, size)
		for j := range want[i] {
			want[i][j] = byte(i*17 + j*31)
		}
		ctx.Prepare(rows[i], want[i])
	}
	set, err := NewBufferSet(ctx, rows)
	require.NoError(t, err)
	defer set.Close()
	var ops []Operation
	for i := 0; i < 8; i++ {
		ops = append(ops, Operation{Dst: uint32(i), Src: uint32(i + 8), Coeff: 37})
	}
	for i := 0; i < 8; i++ {
		ops = append(ops, Operation{Dst: uint32(i + 8), Src: uint32(i), Coeff: 1})
	}
	// These overlapping ranges are sequential dependencies, not SIMD batches.
	for i := 0; i < 7; i++ {
		ops = append(ops, Operation{Dst: uint32(i), Src: uint32(i + 1), Coeff: 1})
	}
	for i := 0; i < 7; i++ {
		ops = append(ops, Operation{Dst: uint32(i + 1), Src: uint32(i), Coeff: 7})
	}
	ops = append(ops, Operation{Dst: 0, Src: 0, Kind: OpScale, Coeff: 0})
	for i := 12; i < count; i++ {
		ops = append(ops, Operation{Dst: uint32(i), Kind: OpClear})
	}
	for _, op := range ops {
		switch op.Kind {
		case OpAdd:
			gf2p16.MulAndAddByteSliceLE(gf2p16.T(op.Coeff), want[op.Src], want[op.Dst])
		case OpScale:
			gf2p16.MulByteSliceLE(gf2p16.T(op.Coeff), want[op.Src], want[op.Dst])
		case OpClear:
			clear(want[op.Dst])
		}
	}
	require.NoError(t, set.Apply(ctx, ops))
	for i := range rows {
		got := make([]byte, size)
		ctx.Finish(rows[i], got)
		require.Equal(t, want[i], got, "row %d", i)
	}
}

func TestOwnedBufferWorkspace(t *testing.T) {
	ctx, err := NewContext(130)
	require.NoError(t, err)
	defer ctx.Close()
	set, err := NewBufferWorkspace(ctx, 4)
	require.NoError(t, err)
	defer set.Close()
	require.Len(t, set.Buffers(), 4)
	for _, row := range set.Buffers() {
		got := make([]byte, 130)
		ctx.Finish(row, got)
		require.Equal(t, make([]byte, 130), got)
	}
	set.Close()
	require.Nil(t, set.Buffers())
	require.Error(t, set.Apply(ctx, nil))
	_, err = NewBufferWorkspace(ctx, 0)
	require.Error(t, err)
}

func TestOversizedBufferWorkspace(t *testing.T) {
	ctx, err := NewContext(130)
	require.NoError(t, err)
	defer ctx.Close()
	_, err = NewBufferWorkspace(ctx, int(^uint(0)>>1))
	require.Error(t, err)
}

func TestBufferSetButterflies(t *testing.T) {
	for _, size := range []int{2, 130, 4096} {
		ctx, err := NewContext(size)
		require.NoError(t, err)
		rows := ctx.NewBuffers(4)
		set, err := NewBufferSet(ctx, rows)
		require.NoError(t, err)
		a, b := make([]byte, size), make([]byte, size)
		for i := range a {
			a[i] = byte(i*73 + 17)
			b[i] = byte(i*31 + 7)
		}
		for _, c := range []uint16{0, 1, 2, 255, 256, 8191, 32768, 65535} {
			for i := 0; i < 2; i++ {
				ctx.Prepare(rows[i], a)
				ctx.Prepare(rows[i+2], b)
			}
			ops := []Operation{{Dst: 0, Src: 2, Coeff: c, Kind: OpButterfly}, {Dst: 1, Src: 3, Coeff: c, Kind: OpButterfly}}
			require.NoError(t, set.Apply(ctx, ops))
			wantA := append([]byte(nil), a...)
			gf2p16.MulAndAddByteSliceLE(gf2p16.T(c), b, wantA)
			wantB := append([]byte(nil), b...)
			for i := range wantB {
				wantB[i] ^= wantA[i]
			}
			scratch := ctx.NewBuffer()
			got := make([]byte, size)
			for i := 0; i < 4; i++ {
				copy(scratch, rows[i])
				ctx.Finish(scratch, got)
				if i < 2 {
					require.Equal(t, wantA, got)
				} else {
					require.Equal(t, wantB, got)
				}
			}
			for i := range ops {
				ops[i].Kind = OpInverseButterfly
			}
			require.NoError(t, set.Apply(ctx, ops))
			for i := 0; i < 4; i++ {
				copy(scratch, rows[i])
				ctx.Finish(scratch, got)
				if i < 2 {
					require.Equal(t, a, got)
				} else {
					require.Equal(t, b, got)
				}
			}
		}
		require.Error(t, set.Apply(ctx, []Operation{{Dst: 0, Src: 0, Kind: OpButterfly}}))
		set.Close()
		ctx.Close()
	}
}

// Repeated registration must remain safe after the Go buffers from a previous
// pointer table become unreachable and the native allocator reuses its memory.
// Run with GOEXPERIMENT=cgocheck2 GOGC=10 to stress write barriers as well.
func TestBufferSetRegistrationAcrossGC(t *testing.T) {
	ctx, err := NewContext(130)
	require.NoError(t, err)
	defer ctx.Close()
	for iteration := 0; iteration < 64; iteration++ {
		func() {
			rows := ctx.NewBuffers(128)
			set, err := NewBufferSet(ctx, rows)
			require.NoError(t, err)
			defer set.Close()
			raw := bytes.Repeat([]byte{byte(iteration), 19}, 65)
			ctx.Prepare(rows[0], raw)
			require.NoError(t, set.Apply(ctx, []Operation{{Dst: 1, Src: 0, Coeff: 37, Kind: OpScale}}))
			got := make([]byte, 130)
			ctx.Finish(rows[1], got)
			require.Equal(t, refMul(37, raw), got)
		}()
		runtime.GC()
	}
}
