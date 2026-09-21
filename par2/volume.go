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
// packet's set ID, type, body, and absolute offset. Validation (magic, length, packet hash)
// is exactly readNextPacket's, applied per packet. body aliases an internal
// reusable buffer: fn must copy anything it retains.
//
// This exists so recovery volumes — hundreds of megabytes each on large
// sets — never have to be materialised whole just to enumerate packets.
func walkPackets(r io.ReaderAt, size int64, fn func(setID recoverySetID, typ packetType, body []byte, packetOffset int64) error) error {
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
		if err := fn(setID, typ, body, offset); err != nil {
			return err
		}
		offset += length
	}
	return nil
}

// packetRef locates one packet inside a volume and carries its validated
// header.
type packetRef struct {
	offset, length int64
	header         packetHeader
}

// indexPackets walks a volume's packet headers without reading bodies and
// returns every packet's offset and length, validating lengths as
// walkPackets does. It is the cheap sequential half of a parallel walk.
func indexPackets(r io.ReaderAt, size int64) ([]packetRef, error) {
	headerSize := int64(sizeOfPacketHeader())
	var header [64]byte
	var refs []packetRef
	for offset := int64(0); offset < size; {
		if size-offset < headerSize {
			return nil, errors.New("trailing bytes too short for a packet header")
		}
		if _, err := r.ReadAt(header[:], offset); err != nil {
			return nil, err
		}
		h, err := readPacketHeader(bytes.NewBuffer(header[:]))
		if err != nil {
			return nil, err
		}
		length := int64(h.Length)
		if length > maxPacketLength || offset+length > size {
			return nil, errors.New("invalid packet length")
		}
		refs = append(refs, packetRef{offset, length, h})
		offset += length
	}
	return refs, nil
}

// readPacketAt reads and validates the packet at ref, exactly as
// walkPackets does for one packet. buf is scratch and is grown as needed.
func readPacketAt(r io.ReaderAt, ref packetRef, buf *[]byte) (recoverySetID, packetType, []byte, error) {
	if int64(cap(*buf)) < ref.length {
		*buf = make([]byte, ref.length)
	}
	*buf = (*buf)[:ref.length]
	if _, err := r.ReadAt(*buf, ref.offset); err != nil {
		return recoverySetID{}, packetType{}, nil, err
	}
	return readNextPacket(bytes.NewBuffer(*buf))
}

// readRecoveryExponent reads only the 4-byte exponent of the recovery packet
// at ref, returning it as a body readRecoveryPacket accepts plus the length
// of the unread recovery data. The body's hash is not checked.
func readRecoveryExponent(r io.ReaderAt, ref packetRef) ([]byte, int, error) {
	headerSize := int64(sizeOfPacketHeader())
	bodyLen := ref.length - headerSize
	if bodyLen < 4 || bodyLen%4 != 0 {
		return nil, 0, errors.New("invalid recovery data byte count")
	}
	exp := make([]byte, 4)
	if _, err := r.ReadAt(exp, ref.offset+headerSize); err != nil {
		return nil, 0, err
	}
	return exp, int(bodyLen - 4), nil
}
