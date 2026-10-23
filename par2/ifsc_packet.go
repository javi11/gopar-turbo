package par2

import (
	"bytes"
	"crypto/md5"
	"encoding/binary"
	"errors"
	"io"
)

var ifscPacketType = packetType{'P', 'A', 'R', ' ', '2', '.', '0', '\x00', 'I', 'F', 'S', 'C'}

type checksumPair struct {
	MD5   [md5.Size]byte
	CRC32 [4]byte
}

type ifscPacket struct {
	checksumPairs []checksumPair
}

func readIFSCPacket(body []byte) (fileID, ifscPacket, error) {
	buf := bytes.NewBuffer(body)

	var id fileID
	_, err := io.ReadFull(buf, id[:])
	if err != nil {
		return fileID{}, ifscPacket{}, err
	}

	// Both checksums are byte arrays on the wire. Decode their fixed layout
	// directly instead of reflecting over every byte of every slice checksum.
	const checksumPairSize = md5.Size + 4
	if buf.Len() == 0 || buf.Len()%checksumPairSize != 0 {
		return fileID{}, ifscPacket{}, errors.New("invalid size")
	}
	checksumPairs := make([]checksumPair, buf.Len()/checksumPairSize)
	data := buf.Bytes()
	for i := range checksumPairs {
		pair := data[i*checksumPairSize : (i+1)*checksumPairSize]
		copy(checksumPairs[i].MD5[:], pair[:md5.Size])
		copy(checksumPairs[i].CRC32[:], pair[md5.Size:])
	}

	return id, ifscPacket{checksumPairs}, nil
}

func writeIFSCPacket(id fileID, packet ifscPacket) ([]byte, error) {
	if len(packet.checksumPairs) == 0 {
		return nil, errors.New("no checksum pairs to write")
	}

	buf := bytes.NewBuffer(nil)

	err := binary.Write(buf, binary.LittleEndian, id)
	if err != nil {
		return nil, err
	}

	err = binary.Write(buf, binary.LittleEndian, packet.checksumPairs)
	if err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}
