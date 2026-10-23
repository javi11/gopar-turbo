package par2

import (
	"bytes"
	"crypto/md5"
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIFSCPacketRoundTrip(t *testing.T) {
	packet := ifscPacket{
		checksumPairs: []checksumPair{
			{
				[md5.Size]byte{0xa, 0xb},
				[4]byte{0x1, 0x2, 0x3, 0x4},
			},
			{
				[md5.Size]byte{0x1a, 0x1b},
				[4]byte{0x5, 0x6, 0x7, 0x8},
			},
		},
	}
	fileID := fileID{0x1, 0x2}
	packetBytes, err := writeIFSCPacket(fileID, packet)
	require.NoError(t, err)
	roundTripFileID, roundTripPacket, err := readIFSCPacket(packetBytes)
	require.NoError(t, err)
	require.Equal(t, fileID, roundTripFileID)
	require.Equal(t, packet, roundTripPacket)
}

func TestIFSCPacketWireLayoutAndOwnership(t *testing.T) {
	for _, length := range []int{0, 1, 15, 16, 17, 35, 36, 56, 20016} {
		t.Run(fmt.Sprint(length), func(t *testing.T) {
			body := make([]byte, length)
			for i := range body {
				body[i] = byte(i*73 + 19)
			}
			id, packet, err := readIFSCPacket(body)
			if length < 36 || (length-16)%20 != 0 {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			var wantID fileID
			want := make([]checksumPair, (length-16)/20)
			reader := bytes.NewReader(body)
			require.NoError(t, binary.Read(reader, binary.LittleEndian, &wantID))
			require.NoError(t, binary.Read(reader, binary.LittleEndian, want))
			clear(body) // Parsed checksums must survive reuse of a packet buffer.
			require.Equal(t, wantID, id)
			require.Equal(t, want, packet.checksumPairs)
		})
	}
}

func BenchmarkReadIFSCPacket1000Slices(b *testing.B) {
	body := make([]byte, 16+1000*20)
	for i := range body {
		body[i] = byte(i*73 + 19)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for i := 0; i < b.N; i++ {
		if _, _, err := readIFSCPacket(body); err != nil {
			b.Fatal(err)
		}
	}
}
