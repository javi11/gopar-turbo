package par2

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPacketHeaderRoundTrip(t *testing.T) {
	h := packetHeader{
		Magic:         expectedMagic,
		Length:        100,
		Hash:          [16]byte{0x1},
		RecoverySetID: [16]byte{0x2},
		Type:          [16]byte{0x3},
	}

	buf := bytes.NewBuffer(nil)
	err := writePacketHeader(buf, h)
	require.NoError(t, err)
	roundTripHeader, err := readPacketHeader(buf)
	require.NoError(t, err)
	require.Equal(t, h, roundTripHeader)
	require.Equal(t, 0, buf.Len())
}

func TestPacketHeaderTruncationAndOwnedFields(t *testing.T) {
	wire := make([]byte, 64)
	for i := range wire {
		wire[i] = byte(i*37 + 11)
	}
	copy(wire, expectedMagic[:])
	binary.LittleEndian.PutUint64(wire[8:16], 128)
	for n := 0; n < len(wire); n++ {
		buf := bytes.NewBuffer(wire[:n])
		_, err := readPacketHeader(buf)
		if n == 0 {
			require.ErrorIs(t, err, io.EOF)
		} else {
			require.ErrorIs(t, err, io.ErrUnexpectedEOF)
		}
		require.Zero(t, buf.Len())
	}
	var want packetHeader
	require.NoError(t, binary.Read(bytes.NewReader(wire), binary.LittleEndian, &want))
	encoded := append(wire, 9, 8, 7, 6)
	buf := bytes.NewBuffer(encoded)
	got, err := readPacketHeader(buf)
	require.NoError(t, err)
	require.Equal(t, []byte{9, 8, 7, 6}, buf.Bytes())
	clear(encoded[:64])
	require.Equal(t, want, got)
}

func BenchmarkReadPacketHeader(b *testing.B) {
	wire := make([]byte, 64)
	copy(wire, expectedMagic[:])
	binary.LittleEndian.PutUint64(wire[8:16], 128)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := readPacketHeader(bytes.NewBuffer(wire)); err != nil {
			b.Fatal(err)
		}
	}
}

type typedPacket struct {
	packetType packetType
	body       []byte
}

func TestPacketsRoundTrip(t *testing.T) {
	packets := []typedPacket{
		{packetType{0x1}, []byte{0x2, 0x3, 0x0, 0x1}},
		{packetType{0x4}, []byte{0x5, 0x6, 0x1, 0x0}},
	}

	buf := bytes.NewBuffer(nil)
	setID := recoverySetID{0x5}
	for _, p := range packets {
		err := writeNextPacket(buf, setID, p.packetType, p.body)
		require.NoError(t, err)
	}

	var roundTripPackets []typedPacket
	for {
		roundTripSetID, packetType, body, err := readNextPacket(buf)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		require.Equal(t, setID, roundTripSetID)
		roundTripPackets = append(roundTripPackets, typedPacket{packetType, body})
	}

	require.Equal(t, packets, roundTripPackets)
	require.Equal(t, 0, buf.Len())
}
