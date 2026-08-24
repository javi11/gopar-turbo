package par2

import (
	"bytes"
	"crypto/md5"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"reflect"

	"github.com/javi11/gopar-turbo/rsec16"
)

type fileIO interface {
	ReadFile(path string) ([]byte, error)
	// OpenRead returns a reader over path, its size in bytes, and a close
	// function the caller must invoke. It lets the scanner walk a large
	// file without materialising it, and lets repair re-read a single
	// shard by offset.
	//
	// The signature uses only stdlib types so that implementations outside
	// this package satisfy it: Go requires identical method signatures for
	// interface satisfaction.
	OpenRead(path string) (io.ReaderAt, int64, func() error, error)
	FindWithPrefixAndSuffix(prefix, suffix string) ([]string, error)
	WriteFile(path string, data []byte) error
}

type defaultFileIO struct{}

func (io defaultFileIO) ReadFile(path string) ([]byte, error) {
	return ioutil.ReadFile(path)
}

func (defaultFileIO) OpenRead(path string) (io.ReaderAt, int64, func() error, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, nil, err
	}
	return f, fi.Size(), f.Close, nil
}

func (io defaultFileIO) FindWithPrefixAndSuffix(prefix, suffix string) ([]string, error) {
	return filepath.Glob(prefix + "*" + suffix)
}

func (io defaultFileIO) WriteFile(path string, data []byte) error {
	return ioutil.WriteFile(path, data, 0600)
}

type decoderInputFileInfo struct {
	fileID        fileID
	filename      string
	byteCount     int
	sixteenKHash  [md5.Size]byte
	hash          [md5.Size]byte
	checksumPairs []checksumPair
}

func decoderInputFileInfoIDs(infos []decoderInputFileInfo) []fileID {
	fileIDs := make([]fileID, len(infos))
	for i, info := range infos {
		fileIDs[i] = info.fileID
	}
	return fileIDs
}

func makeDecoderInputFileInfos(fileIDs []fileID, fileDescriptionPackets map[fileID]fileDescriptionPacket, ifscPackets map[fileID]ifscPacket) ([]decoderInputFileInfo, error) {
	var decoderInputFileInfos []decoderInputFileInfo
	for _, fileID := range fileIDs {
		descriptionPacket, ok := fileDescriptionPackets[fileID]
		if !ok {
			return nil, errors.New("file description packet not found")
		}
		ifscPacket, ok := ifscPackets[fileID]
		if !ok {
			return nil, errors.New("input file slice checksum packet not found")
		}
		decoderInputFileInfos = append(decoderInputFileInfos, decoderInputFileInfo{
			fileID,
			descriptionPacket.filename,
			descriptionPacket.byteCount,
			descriptionPacket.sixteenKHash,
			descriptionPacket.hash,
			ifscPacket.checksumPairs,
		})
	}

	return decoderInputFileInfos, nil
}

type shardLocation struct {
	fileID fileID
	start  int
}

type shardLocationSet map[shardLocation]bool

type checksumShardLocationMap map[uint32]map[[md5.Size]byte]shardLocationSet

func (m checksumShardLocationMap) put(crc32 uint32, md5Hash [md5.Size]byte, location shardLocation) {
	byCRC, ok := m[crc32]
	if !ok {
		m[crc32] = make(map[[md5.Size]byte]shardLocationSet)
		byCRC = m[crc32]
	}
	byMD5, ok := byCRC[md5Hash]
	if !ok {
		byCRC[md5Hash] = make(shardLocationSet)
		byMD5 = byCRC[md5Hash]
	}
	byMD5[location] = true
}

func (m checksumShardLocationMap) get(crc32 uint32, data []byte) shardLocationSet {
	byCRC := m[crc32]
	if len(byCRC) == 0 {
		return nil
	}
	return byCRC[md5.Sum(data)]
}

func makeChecksumShardLocationMap(sliceByteCount int, infos []decoderInputFileInfo) checksumShardLocationMap {
	m := make(checksumShardLocationMap)

	for _, info := range infos {
		for i, checksumPair := range info.checksumPairs {
			// TODO: Handle overflow.
			start := i * sliceByteCount
			m.put(binary.LittleEndian.Uint32(checksumPair.CRC32[:]), checksumPair.MD5, shardLocation{info.fileID, start})
		}
	}

	return m
}

type shardIntegrityInfo struct {
	// present is true when the shard's bytes were located on disk. The
	// bytes themselves are not retained: readShard re-reads them from
	// foundAt when they are actually needed.
	present bool
	// foundAt is where the bytes actually live, which is not necessarily
	// the shard's canonical offset.
	foundAt   shardLocation
	locations shardLocationSet
}

func (info shardIntegrityInfo) ok(location shardLocation) bool {
	return info.present && info.locations[location]
}

type fileIntegrityInfo struct {
	fileID            fileID
	missing           bool
	hashMismatch      bool
	hasWrongByteCount bool
	shardInfos        []shardIntegrityInfo
}

func (info fileIntegrityInfo) allShardsOK(sliceByteCount int) bool {
	for i, shardInfo := range info.shardInfos {
		// TODO: Handle overflow.
		start := i * sliceByteCount
		if !shardInfo.ok(shardLocation{info.fileID, start}) {
			return false
		}
	}
	return true
}

func (info fileIntegrityInfo) ok(sliceByteCount int) bool {
	return !info.missing && !info.hashMismatch && !info.hasWrongByteCount && info.allShardsOK(sliceByteCount)
}

// A Decoder keeps track of all information needed to check the
// integrity of a set of data files, and possibly repair any
// missing/corrupt data files from the parity files (that usually end
// in .par2).
type Decoder struct {
	fileIO   fileIO
	delegate DecoderDelegate

	indexPath string

	setID          recoverySetID
	clientID       string
	sliceByteCount int
	recoverySet    []decoderInputFileInfo
	nonRecoverySet []decoderInputFileInfo

	numGoroutines int
	scanPolicy    scanPolicy
	// memoryBudget caps the bytes held for reconstruction accumulators.
	// Zero selects defaultMemoryBudget().
	memoryBudget int

	// fileIDIndices maps a file ID to its index in recoverySet, so a shard
	// can be traced back to the file its bytes live in.
	fileIDIndices map[fileID]int

	checksumToLocation checksumShardLocationMap

	// Indexed the same as recoverySet.
	fileIntegrityInfos []fileIntegrityInfo

	parityShards [][]byte
	// parityPresent[i] records that parity shard i exists, even when its
	// bytes were not retained.
	parityPresent []bool
}

// DecoderDelegate holds methods that are called during the decode
// process.
type DecoderDelegate interface {
	OnCreatorPacketLoad(clientID string)
	OnMainPacketLoad(sliceByteCount, recoverySetCount, nonRecoverySetCount int)
	OnFileDescriptionPacketLoad(fileID [16]byte, filename string, byteCount int)
	OnIFSCPacketLoad(fileID [16]byte)
	OnRecoveryPacketLoad(exponent uint16, byteCount int)
	OnUnknownPacketLoad(packetType [16]byte, byteCount int)
	OnOtherPacketSkip(setID [16]byte, packetType [16]byte, byteCount int)
	OnDataFileLoad(i, n int, path string, byteCount, hits, misses int, err error)
	OnParityFileLoad(i int, path string, err error)
	OnDetectCorruptDataChunk(fileID [16]byte, path string, startByteOffset, endByteOffset int)
	OnDetectDataFileHashMismatch(fileID [16]byte, path string)
	OnDetectDataFileWrongByteCount(fileID [16]byte, path string)
	OnDataFileWrite(i, n int, path string, byteCount int, err error)
}

// DoNothingDecoderDelegate is an implementation of DecoderDelegate
// that does nothing for all methods.
type DoNothingDecoderDelegate struct{}

// OnCreatorPacketLoad implements the DecoderDelegate interface.
func (DoNothingDecoderDelegate) OnCreatorPacketLoad(clientID string) {}

// OnMainPacketLoad implements the DecoderDelegate interface.
func (DoNothingDecoderDelegate) OnMainPacketLoad(sliceByteCount, recoverySetCount, nonRecoverySetCount int) {
}

// OnFileDescriptionPacketLoad implements the DecoderDelegate interface.
func (DoNothingDecoderDelegate) OnFileDescriptionPacketLoad(fileID [16]byte, filename string, byteCount int) {
}

// OnIFSCPacketLoad implements the DecoderDelegate interface.
func (DoNothingDecoderDelegate) OnIFSCPacketLoad(fileID [16]byte) {}

// OnRecoveryPacketLoad implements the DecoderDelegate interface.
func (DoNothingDecoderDelegate) OnRecoveryPacketLoad(exponent uint16, byteCount int) {}

// OnUnknownPacketLoad implements the DecoderDelegate interface.
func (DoNothingDecoderDelegate) OnUnknownPacketLoad(packetType [16]byte, byteCount int) {}

// OnOtherPacketSkip implements the DecoderDelegate interface.
func (DoNothingDecoderDelegate) OnOtherPacketSkip(setID [16]byte, packetType [16]byte, byteCount int) {
}

// OnDataFileLoad implements the DecoderDelegate interface.
func (DoNothingDecoderDelegate) OnDataFileLoad(i, n int, path string, byteCount, hits, misses int, err error) {
}

// OnParityFileLoad implements the DecoderDelegate interface.
func (DoNothingDecoderDelegate) OnParityFileLoad(i int, path string, err error) {}

// OnDetectCorruptDataChunk implements the DecoderDelegate interface.
func (DoNothingDecoderDelegate) OnDetectCorruptDataChunk(fileID [16]byte, path string, startByteOffset, endByteOffset int) {
}

// OnDetectDataFileHashMismatch implements the DecoderDelegate interface.
func (DoNothingDecoderDelegate) OnDetectDataFileHashMismatch(fileID [16]byte, path string) {}

// OnDetectDataFileWrongByteCount implements the DecoderDelegate interface.
func (DoNothingDecoderDelegate) OnDetectDataFileWrongByteCount(fileID [16]byte, path string) {}

// OnDataFileWrite implements the DecoderDelegate interface.
func (DoNothingDecoderDelegate) OnDataFileWrite(i, n int, path string, byteCount int, err error) {}

func newDecoder(fileIO fileIO, delegate DecoderDelegate, indexPath string, numGoroutines int, policy scanPolicy, memoryBudget int) (*Decoder, error) {
	indexBytes, err := fileIO.ReadFile(indexPath)
	if err != nil {
		return nil, err
	}

	setID, indexFile, err := readFile(delegate, nil, indexBytes)
	if err != nil {
		return nil, err
	}

	if indexFile.mainPacket == nil {
		// TODO: Relax this check.
		return nil, errors.New("no main packet found")
	}

	if len(indexFile.recoveryPackets) > 0 {
		// TODO: Relax this check.
		return nil, errors.New("recovery packets found in index file")
	}

	recoverySet, err := makeDecoderInputFileInfos(indexFile.mainPacket.recoverySet, indexFile.fileDescriptionPackets, indexFile.ifscPackets)
	if err != nil {
		return nil, err
	}

	nonRecoverySet, err := makeDecoderInputFileInfos(indexFile.mainPacket.nonRecoverySet, indexFile.fileDescriptionPackets, indexFile.ifscPackets)
	if err != nil {
		return nil, err
	}

	return &Decoder{
		fileIO:         fileIO,
		delegate:       delegate,
		indexPath:      indexPath,
		setID:          setID,
		clientID:       indexFile.clientID,
		sliceByteCount: indexFile.mainPacket.sliceByteCount,
		recoverySet:    recoverySet,
		nonRecoverySet: nonRecoverySet,
		numGoroutines:  numGoroutines,
		scanPolicy:     policy,
		memoryBudget:   memoryBudget,
	}, nil
}

func sixteenKHash(data []byte) [md5.Size]byte {
	if len(data) < 16*1024 {
		return md5.Sum(data)
	}
	return md5.Sum(data[:16*1024])
}

func sliceAndPadByteArray(bs []byte, start, end int) []byte {
	padLength := 0
	if end > len(bs) {
		padLength = end - len(bs)
		end = len(bs)
	}
	slice := bs[start:end]
	if padLength > 0 {
		slice = append(slice, make([]byte, padLength)...)
	}
	return slice
}

// scanPolicy controls how the scanner reacts to a slice-aligned chunk that
// matches no known shard checksum.
type scanPolicy struct {
	// findMisaligned enables the byte-wise rolling-CRC32 search that locates
	// shards sitting off a slice boundary (data shifted by an insertion or
	// deletion). It is expensive: every miss can slide up to a whole slice
	// before the scan resynchronises. When false, a miss simply advances to
	// the next slice boundary.
	//
	// par2cmdline exposes the same trade-off as its -N flag, off by default.
	findMisaligned bool

	// searchLimit bounds how far the byte-wise search slides past a slice
	// boundary before giving up on that slice. Zero means unbounded, which
	// is what findMisaligned alone has always meant.
	searchLimit int
}

// defaultScanPolicy skips the misaligned-data search, matching par2cmdline's
// default. Callers that need it opt in explicitly.
func defaultScanPolicy() scanPolicy {
	return scanPolicy{}
}

// fillShardInfos scans a whole buffer. It is kept for tests and for callers
// that already hold the entire file.
func fillShardInfos(sliceByteCount int, data []byte, checksumToLocation checksumShardLocationMap, fileID fileID, fileIntegrityInfos []fileIntegrityInfo, fileIDIndices map[fileID]int, policy scanPolicy) (int, int) {
	_, hits, misses := scanBuffer(sliceByteCount, data, len(data), 0, checksumToLocation, fileID, fileIntegrityInfos, fileIDIndices, policy)
	return hits, misses
}

// scanBuffer scans buf for known shards, treating baseOffset as the file
// offset of buf[0]. It keeps scanning while the cursor is below limit, so a
// caller streaming a file can set limit short of the end and carry the
// remainder into the next window. It returns the cursor position where it
// stopped, plus hit and miss counts.
func scanBuffer(sliceByteCount int, data []byte, limit, baseOffset int, checksumToLocation checksumShardLocationMap, fileID fileID, fileIntegrityInfos []fileIntegrityInfo, fileIDIndices map[fileID]int, policy scanPolicy) (int, int, int) {
	hits := 0
	misses := 0

	justMissed := false
	window := newCRC32Window(sliceByteCount)
	var crcSlice uint32
	// slideStart is the slice boundary the current byte-wise search began
	// from. Only meaningful while justMissed is true.
	slideStart := 0
	j := 0
	for j < limit {
		slice := sliceAndPadByteArray(data, j, j+sliceByteCount)
		if justMissed {
			crcSlice = window.update(crcSlice, data[j-1], slice[len(slice)-1])
		} else {
			crcSlice = crc32.ChecksumIEEE(slice)
		}
		foundLocations := checksumToLocation.get(crcSlice, slice)
		if len(foundLocations) == 0 {
			misses++

			// Without the misaligned search, a miss just means this
			// slice is damaged; move on to the next boundary.
			if !policy.findMisaligned {
				j += sliceByteCount
				justMissed = false
				continue
			}

			if !justMissed {
				slideStart = j
			}
			j++

			// Give up on this slice once the search has slid past the
			// caller's limit, and resume from the next boundary. The
			// rolling window is no longer contiguous after the jump, so
			// the next CRC must be computed from scratch.
			if policy.searchLimit > 0 && j-slideStart >= policy.searchLimit {
				j = slideStart + sliceByteCount
				justMissed = false
				continue
			}

			justMissed = true
			continue
		}

		location := shardLocation{fileID, baseOffset + j}
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

		justMissed = false
		j += sliceByteCount
		hits++
	}

	return j, hits, misses
}

func (d *Decoder) getFilePath(info decoderInputFileInfo) string {
	// TODO: Make this configurable.
	basePath := filepath.Dir(d.indexPath)
	return filepath.Join(basePath, info.filename)
}

// scanWindowSize returns the buffer size for the streaming scan. It is a whole
// number of slices, at least two so the misaligned search can slide a full
// slice past a boundary without re-reading, and at least a megabyte so reads
// stay efficient when slices are small.
func scanWindowSize(sliceByteCount int) int {
	const minWindow = 1 << 20
	slices := 2
	if n := minWindow/sliceByteCount + 1; n > slices {
		slices = n
	}
	return slices * sliceByteCount
}

func (d *Decoder) fillFileIntegrityInfos(checksumToLocation checksumShardLocationMap, fileIntegrityInfos []fileIntegrityInfo, fileIDIndices map[fileID]int, i int, info decoderInputFileInfo) (int, int, int, error) {
	path := d.getFilePath(info)
	r, size, closeFn, err := d.fileIO.OpenRead(path)
	if os.IsNotExist(err) {
		fileIntegrityInfos[i].missing = true
		return 0, 0, 0, nil
	} else if err != nil {
		return 0, 0, 0, err
	}
	defer closeFn()

	windowSize := scanWindowSize(d.sliceByteCount)
	buf := make([]byte, windowSize)

	fullHash := md5.New()
	head := make([]byte, 0, 16*1024)

	hits, misses := 0, 0
	carry := 0           // bytes retained at the front of buf from the last window
	var readOffset int64 // next unread byte in the file
	windowStart := 0     // file offset of buf[0]

	for {
		n := windowSize - carry
		if remaining := size - readOffset; int64(n) > remaining {
			n = int(remaining)
		}
		if n > 0 {
			if _, err := r.ReadAt(buf[carry:carry+n], readOffset); err != nil {
				return int(size), hits, misses, err
			}
			fullHash.Write(buf[carry : carry+n])
			if len(head) < cap(head) {
				head = append(head, buf[carry : carry+n][:min(n, cap(head)-len(head))]...)
			}
			readOffset += int64(n)
		}

		window := buf[:carry+n]
		if len(window) == 0 {
			break
		}

		atEOF := readOffset >= size
		limit := len(window)
		if !atEOF {
			limit -= d.sliceByteCount
			if limit < 0 {
				limit = 0
			}
		}

		consumed, h, m := scanBuffer(d.sliceByteCount, window, limit, windowStart,
			checksumToLocation, info.fileID, fileIntegrityInfos, fileIDIndices, d.scanPolicy)
		hits += h
		misses += m

		if atEOF {
			break
		}

		// Carry the unscanned tail to the front of the next window.
		carry = len(window) - consumed
		copy(buf, window[consumed:])
		windowStart += consumed
	}

	var full [md5.Size]byte
	copy(full[:], fullHash.Sum(nil))
	sixteenK := md5.Sum(head)

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

// LoadFileData loads existing file data into memory.
func (d *Decoder) LoadFileData() error {
	checksumToLocation := makeChecksumShardLocationMap(d.sliceByteCount, d.recoverySet)

	fileIntegrityInfos := make([]fileIntegrityInfo, len(d.recoverySet))
	fileIDIndices := make(map[fileID]int)
	for i, info := range d.recoverySet {
		fileIntegrityInfos[i] = fileIntegrityInfo{
			fileID:     info.fileID,
			shardInfos: make([]shardIntegrityInfo, len(info.checksumPairs)),
		}
		fileIDIndices[info.fileID] = i
	}
	d.fileIDIndices = fileIDIndices

	for i, info := range d.recoverySet {
		path := d.getFilePath(info)
		byteCount, hits, misses, err := d.fillFileIntegrityInfos(checksumToLocation, fileIntegrityInfos, fileIDIndices, i, info)
		d.delegate.OnDataFileLoad(i+1, len(d.recoverySet), path, byteCount, hits, misses, err)
		if err != nil {
			return err
		}

		if byteCount != info.byteCount {
			var startByteOffset, endByteOffset int
			if byteCount < info.byteCount {
				startByteOffset = byteCount
				endByteOffset = info.byteCount
			} else {
				startByteOffset = info.byteCount
				endByteOffset = byteCount
			}
			d.delegate.OnDetectCorruptDataChunk(info.fileID, path, startByteOffset, endByteOffset)
		}
	}

	for i, info := range d.recoverySet {
		integrityInfo := fileIntegrityInfos[i]
		corruptStartByteOffset := -1
		corruptEndByteOffset := -1
		for j, shardInfo := range integrityInfo.shardInfos {
			startByteOffset := j * d.sliceByteCount
			endByteOffset := startByteOffset + d.sliceByteCount
			if endByteOffset > info.byteCount {
				endByteOffset = info.byteCount
			}
			if shardInfo.ok(shardLocation{info.fileID, startByteOffset}) {
				if corruptStartByteOffset != -1 {
					d.delegate.OnDetectCorruptDataChunk(info.fileID, d.getFilePath(info), corruptStartByteOffset, corruptEndByteOffset)
					corruptStartByteOffset = -1
					corruptEndByteOffset = -1
				}
			} else {
				if corruptStartByteOffset == -1 {
					corruptStartByteOffset = startByteOffset
				}
				corruptEndByteOffset = endByteOffset
			}
		}

		if corruptStartByteOffset != -1 {
			d.delegate.OnDetectCorruptDataChunk(info.fileID, d.getFilePath(info), corruptStartByteOffset, info.byteCount)
		}
	}

	d.checksumToLocation = checksumToLocation
	d.fileIntegrityInfos = fileIntegrityInfos
	return nil
}

type recoveryDelegate struct {
	d DecoderDelegate
}

func (recoveryDelegate) OnCreatorPacketLoad(clientID string) {}

func (recoveryDelegate) OnMainPacketLoad(sliceByteCount, recoverySetCount, nonRecoverySetCount int) {}

func (recoveryDelegate) OnFileDescriptionPacketLoad(fileID [16]byte, filename string, byteCount int) {
}

func (recoveryDelegate) OnIFSCPacketLoad(fileID [16]byte) {}

func (r recoveryDelegate) OnRecoveryPacketLoad(exponent uint16, byteCount int) {
	r.d.OnRecoveryPacketLoad(exponent, byteCount)
}

func (r recoveryDelegate) OnUnknownPacketLoad(packetType [16]byte, byteCount int) {
	r.d.OnUnknownPacketLoad(packetType, byteCount)
}

func (recoveryDelegate) OnOtherPacketSkip(setID [16]byte, packetType [16]byte, byteCount int) {}

func (recoveryDelegate) OnDataFileLoad(i, n int, path string, byteCount, hits, misses int, err error) {
}

func (recoveryDelegate) OnParityFileLoad(i int, path string, err error) {}

func (recoveryDelegate) OnDetectCorruptDataChunk(fileID [16]byte, path string, startByteOffset, endByteOffset int) {
}

func (recoveryDelegate) OnDetectDataFileHashMismatch(fileID [16]byte, path string) {}

func (recoveryDelegate) OnDetectDataFileWrongByteCount(fileID [16]byte, path string) {}

func (recoveryDelegate) OnDataFileWrite(i, n int, path string, byteCount int, err error) {}

// LoadParityData searches for parity volumes and loads them into
// memory.
func (d *Decoder) LoadParityData() error {
	return d.loadParityData(true)
}

// LoadParityPresence records which parity shards exist without retaining their
// bytes. That is all verification needs, and it keeps verify memory
// independent of the size of the recovery set.
func (d *Decoder) LoadParityPresence() error {
	return d.loadParityData(false)
}

func (d *Decoder) loadParityData(retain bool) error {
	ext := path.Ext(d.indexPath)
	base := d.indexPath[:len(d.indexPath)-len(ext)]
	matches, err := d.fileIO.FindWithPrefixAndSuffix(base+".", ext)
	if err != nil {
		return err
	}

	var parityPresent []bool
	var parityShards [][]byte
	for i, match := range matches {
		parityFile, err := func() (*file, error) {
			volumeBytes, err := d.fileIO.ReadFile(match)
			if err != nil {
				return nil, err
			}

			// Ignore all the other packet types other
			// than recovery packets.
			_, parityFile, err := readFile(recoveryDelegate{d.delegate}, &d.setID, volumeBytes)
			if _, ok := err.(noPacketsFoundError); ok {
				return nil, nil
			} else if err != nil {
				// TODO: Relax this check.
				return nil, err
			}

			if d.sliceByteCount != parityFile.mainPacket.sliceByteCount {
				return nil, errors.New("slice byte count mismatch")
			}

			if !reflect.DeepEqual(decoderInputFileInfoIDs(d.recoverySet), parityFile.mainPacket.recoverySet) {
				return nil, errors.New("recovery set mismatch")
			}

			if !reflect.DeepEqual(decoderInputFileInfoIDs(d.nonRecoverySet), parityFile.mainPacket.nonRecoverySet) {
				return nil, errors.New("non-recovery set mismatch")
			}

			return &parityFile, nil
		}()
		d.delegate.OnParityFileLoad(i+1, match, err)
		if err != nil {
			return err
		}
		if parityFile == nil {
			continue
		}

		for exponent, packet := range parityFile.recoveryPackets {
			if int(exponent) >= len(parityPresent) {
				grow := int(exponent+1) - len(parityPresent)
				parityPresent = append(parityPresent, make([]bool, grow)...)
				parityShards = append(parityShards, make([][]byte, grow)...)
			}
			parityPresent[exponent] = true
			if retain {
				// Copy out of the volume buffer so the buffer itself can be
				// collected instead of being pinned by this subslice.
				shard := make([]byte, len(packet.data))
				copy(shard, packet.data)
				parityShards[exponent] = shard
			}
		}
	}

	d.parityPresent = parityPresent
	d.parityShards = parityShards
	return nil
}

// readShard fills buf with the bytes of the given shard, re-reading them from
// wherever the scan found them. buf must be sliceByteCount long; a trailing
// partial slice is zero-padded, matching sliceAndPadByteArray.
func (d *Decoder) readShard(info shardIntegrityInfo, buf []byte) error {
	if !info.present {
		return errors.New("shard not present")
	}
	idx, ok := d.fileIDIndices[info.foundAt.fileID]
	if !ok {
		return errors.New("unknown file for shard")
	}
	path := d.getFilePath(d.recoverySet[idx])
	r, size, closeFn, err := d.fileIO.OpenRead(path)
	if err != nil {
		return err
	}
	defer closeFn()

	for i := range buf {
		buf[i] = 0
	}
	n := int64(len(buf))
	if start := int64(info.foundAt.start); start+n > size {
		n = size - start
	}
	if n <= 0 {
		return nil
	}
	_, err = r.ReadAt(buf[:n], int64(info.foundAt.start))
	return err
}

// shardBytes returns a freshly allocated copy of a shard's bytes. Callers that
// loop should prefer readShard with a reused buffer.
func (d *Decoder) shardBytes(info shardIntegrityInfo) ([]byte, error) {
	buf := make([]byte, d.sliceByteCount)
	if err := d.readShard(info, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// ShardCounts contains shard counts which can be used to deduce
// whether repair is necessary and/or possible.
type ShardCounts struct {
	// UsableDataShardCount is the number of data shards that are
	// usable, i.e. not missing and not corrupt.
	UsableDataShardCount int
	// UnusableDataShardCount is the number of data shards that
	// are unusable, i.e. missing or corrupt.
	UnusableDataShardCount int

	// UsableParityShardCount is the number of parity shards that
	// exist, i.e. not missing and not corrupt.
	UsableParityShardCount int
	// UnusableDataShardCount is the number of parity shards that
	// are unusable, i.e. missing or corrupt.
	UnusableParityShardCount int
}

// RepairNeeded returns whether repair is needed, i.e. whether
// UnusableDataShardCount is non-zero.
func (fc ShardCounts) RepairNeeded() bool {
	return fc.UnusableDataShardCount > 0
}

// RepairPossible returns whether repair is possible i.e. whether
// UsableParityShardCount >= UnusableDataShardCount.
func (fc ShardCounts) RepairPossible() bool {
	return fc.UsableParityShardCount >= fc.UnusableDataShardCount
}

// ShardCounts returns a ShardCounts object for the current shard set.
func (d *Decoder) ShardCounts() ShardCounts {
	usableDataShardCount := 0
	unusableDataShardCount := 0

	for _, info := range d.fileIntegrityInfos {
		for _, shardInfo := range info.shardInfos {
			if !shardInfo.present {
				unusableDataShardCount++
			} else {
				usableDataShardCount++
			}
		}
	}

	usableParityShardCount := 0
	unusableParityShardCount := 0

	for _, present := range d.parityPresent {
		if present {
			usableParityShardCount++
		} else {
			unusableParityShardCount++
		}
	}

	return ShardCounts{
		UsableDataShardCount:     usableDataShardCount,
		UnusableDataShardCount:   unusableDataShardCount,
		UsableParityShardCount:   usableParityShardCount,
		UnusableParityShardCount: unusableParityShardCount,
	}
}

// Repair tries to repair any missing or corrupt data, using the
// parity volumes. Returns a list of paths to files that were
// successfully repaired (relative to the indexFile passed to
// NewDecoder) in no particular order, which is present even if an
// error is returned. If checkParity is true, extra checking is done
// of the reconstructed parity data.
func (d *Decoder) Repair(checkParity bool) ([]string, error) {
	reconstructed, err := d.reconstructMissing()
	if err != nil {
		return nil, err
	}

	if checkParity {
		if err := d.verifyParity(reconstructed); err != nil {
			return nil, err
		}
	}

	wasOK := make([]bool, len(d.fileIntegrityInfos))
	for i, info := range d.fileIntegrityInfos {
		wasOK[i] = info.ok(d.sliceByteCount)
	}

	cache := newReaderCache(d)
	defer cache.Close()

	// Shards are read from disk on demand, so rewriting one file can destroy
	// the source of another's shards: with two files' contents swapped, each
	// file's shards live inside the other. Pre-read any surviving shard whose
	// source file is itself scheduled to be rewritten. In the common case
	// nothing qualifies and this costs nothing.
	if err := d.preserveShardsBeforeOverwrite(cache, reconstructed, wasOK); err != nil {
		return nil, err
	}

	// Reuse one assembly buffer across files. Allocating a fresh one per
	// file turned every repaired file into garbage the collector had to
	// chase, which showed up as peak RSS several times the live heap.
	maxShards := 0
	for _, info := range d.fileIntegrityInfos {
		if n := len(info.shardInfos); n > maxShards {
			maxShards = n
		}
	}
	assembly := make([]byte, maxShards*d.sliceByteCount)
	shardBuf := make([]byte, d.sliceByteCount)

	var repairedPaths []string
	shardBase := 0
	for i, inputFileInfo := range d.recoverySet {
		info := d.fileIntegrityInfos[i]
		shardCount := len(info.shardInfos)
		if wasOK[i] {
			shardBase += shardCount
			continue
		}

		// Assemble the file one shard at a time: survivors come off disk,
		// rebuilt shards from the reconstruction.
		for j := 0; j < shardCount; j++ {
			if err := d.shardSource(cache, reconstructed, shardBase+j, info.shardInfos[j], shardBuf); err != nil {
				return repairedPaths, err
			}
			copy(assembly[j*d.sliceByteCount:], shardBuf)
		}
		data := assembly[:inputFileInfo.byteCount]

		if sixteenKHash(data) != inputFileInfo.sixteenKHash {
			return repairedPaths, errors.New("hash mismatch (16k) in reconstructed data")
		} else if md5.Sum(data) != inputFileInfo.hash {
			return repairedPaths, errors.New("hash mismatch in reconstructed data")
		}

		path := d.getFilePath(inputFileInfo)
		err = d.fileIO.WriteFile(path, data)
		d.delegate.OnDataFileWrite(i+1, len(d.recoverySet), path, len(data), err)
		if err != nil {
			return repairedPaths, err
		}
		repairedPaths = append(repairedPaths, path)

		// The file is now whole: every shard sits at its canonical offset.
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

		shardBase += shardCount
	}

	// TODO: Repair missing parity volumes, too, and then make
	// sure d.Verify() passes.

	return repairedPaths, nil
}

// preserveShardsBeforeOverwrite reads, into reconstructed, every surviving
// shard that a file needing repair sources from another file that is also
// about to be rewritten.
func (d *Decoder) preserveShardsBeforeOverwrite(cache *readerCache, reconstructed map[int][]byte, wasOK []bool) error {
	willRewrite := make(map[fileID]bool)
	for i, info := range d.fileIntegrityInfos {
		if !wasOK[i] {
			willRewrite[info.fileID] = true
		}
	}
	if len(willRewrite) == 0 {
		return nil
	}

	shardBase := 0
	for i, info := range d.fileIntegrityInfos {
		if wasOK[i] {
			shardBase += len(info.shardInfos)
			continue
		}
		for j, si := range info.shardInfos {
			idx := shardBase + j
			if _, done := reconstructed[idx]; done {
				continue
			}
			// A shard sourced from its own file is safe: the file is
			// fully assembled in memory before it is written. Only a
			// shard living in a *different* file that is also being
			// rewritten can have its source destroyed first.
			if !si.present || si.foundAt.fileID == info.fileID {
				continue
			}
			if !willRewrite[si.foundAt.fileID] {
				continue
			}
			buf := make([]byte, d.sliceByteCount)
			if err := cache.readRange(si, 0, buf); err != nil {
				return err
			}
			reconstructed[idx] = buf
		}
		shardBase += len(info.shardInfos)
	}
	return nil
}

// verifyParity recomputes the recovery blocks from the repaired data and
// compares them against the ones on disk. It folds the data shards in the same
// bounded way reconstruction does, so the double check does not undo the
// memory saving it is checking.
func (d *Decoder) verifyParity(reconstructed map[int][]byte) error {
	coder, _, _, _, _, err := d.planReconstruction()
	if err != nil {
		return err
	}

	var dataShards []shardIntegrityInfo
	for _, info := range d.fileIntegrityInfos {
		dataShards = append(dataShards, info.shardInfos...)
	}

	cache := newReaderCache(d)
	defer cache.Close()

	parityCount := len(d.parityShards)
	chunk := chunkSizeFor(d.memoryBudget, d.sliceByteCount, parityCount)
	out := make([][]byte, parityCount)

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

		buf := make([]byte, d.sliceByteCount)
		err := rsec16.FoldInputs(coder.ParityMatrix(), len(dataShards), n, func(j int, dst []byte) error {
			if err := d.shardSource(cache, reconstructed, j, dataShards[j], buf); err != nil {
				return err
			}
			end := off + n
			if end > len(buf) {
				end = len(buf)
			}
			copy(dst, buf[off:end])
			return nil
		}, out, d.numGoroutines)
		if err != nil {
			return err
		}

		for i, shard := range d.parityShards {
			if len(shard) == 0 {
				continue
			}
			end := off + n
			if end > len(shard) {
				end = len(shard)
			}
			if !bytes.Equal(out[i][:end-off], shard[off:end]) {
				return errors.New("repair failed")
			}
		}
	}
	return nil
}

// NewDecoder reads the given index file, which usually has a .par2
// extension.
func NewDecoder(delegate DecoderDelegate, indexFile string, numGoroutines int) (*Decoder, error) {
	return newDecoder(defaultFileIO{}, delegate, indexFile, numGoroutines, defaultScanPolicy(), 0)
}
