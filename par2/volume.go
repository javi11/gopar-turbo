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
// is exactly readNextPacket's, applied per packet. body aliases an internal
// reusable buffer: fn must copy anything it retains.
//
// This exists so recovery volumes — hundreds of megabytes each on large
// sets — never have to be materialised whole just to enumerate packets.
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
		// full read; readNextPacket re-validates the whole header.
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
