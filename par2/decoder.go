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
	"sort"
	"sync"

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
	// OpenWrite starts a size-byte write of path and returns a positional
	// writer plus a close function the caller must invoke exactly once:
	// close(true) commits the result to path, close(false) discards it and
	// leaves path as it was. It lets repair write reconstructed chunk
	// ranges directly to their final offsets instead of assembling whole
	// files in memory, without destroying data still being read out of the
	// file being replaced.
	OpenWrite(path string, size int64) (io.WriterAt, func(commit bool) error, error)
	FindWithPrefixAndSuffix(prefix, suffix string) ([]string, error)
	WriteFile(path string, data []byte) error
}

// patchFileIO is optionally implemented by a fileIO that can start a
// rewrite of path from a copy-on-write clone of its current contents, so
// that only the slices repair actually reconstructs need to be written.
// The returned writer and close function follow OpenWrite's contract.
// Implementations return an error when the clone cannot be made, and the
// caller falls back to OpenWrite plus copying the surviving slices.
type patchFileIO interface {
	OpenPatch(path string, size int64) (io.WriterAt, func(commit bool) error, error)
}

type defaultFileIO struct{}

// OpenPatch clones path to a sibling temp file (APFS clonefile, Linux
// reflink), truncates or extends it to size, and returns a writer over it
// with the same commit-by-rename close as OpenWrite. Untouched ranges of
// the clone share blocks with the original until written.
func (defaultFileIO) OpenPatch(path string, size int64) (io.WriterAt, func(commit bool) error, error) {
	tmp := path + ".gopar-tmp"
	_ = os.Remove(tmp)
	if err := cloneFile(path, tmp); err != nil {
		return nil, nil, err
	}
	f, err := os.OpenFile(tmp, os.O_RDWR, 0600)
	if err != nil {
		os.Remove(tmp)
		return nil, nil, err
	}
	if err := f.Truncate(size); err != nil {
		f.Close()
		os.Remove(tmp)
		return nil, nil, err
	}
	return f, tempCommitCloser(f, tmp, path), nil
}

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

func (defaultFileIO) OpenWrite(path string, size int64) (io.WriterAt, func(commit bool) error, error) {
	// Write to a sibling temp file and rename on close. Truncating path
	// itself would destroy the surviving shards still being read out of it,
	// and it would leave a half-written file behind on failure.
	tmp := path + ".gopar-tmp"
	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return nil, nil, err
	}
	if err := f.Truncate(size); err != nil {
		f.Close()
		os.Remove(tmp)
		return nil, nil, err
	}
	return f, tempCommitCloser(f, tmp, path), nil
}

// tempCommitCloser closes f and, on commit, renames tmp over path; on
// discard or failure the temp file is removed.
func tempCommitCloser(f *os.File, tmp, path string) func(commit bool) error {
	return func(commit bool) error {
		cerr := f.Close()
		if !commit || cerr != nil {
			os.Remove(tmp)
			return cerr
		}
		if err := os.Rename(tmp, path); err != nil {
			os.Remove(tmp)
			return err
		}
		return nil
	}
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

	// scanMu guards shard-match recording and mid-scan delegate calls when
	// data files are scanned concurrently: a matched slice may belong to a
	// file other than the one being scanned.
	scanMu sync.Mutex

	// fileIDIndices maps a file ID to its index in recoverySet, so a shard
	// can be traced back to the file its bytes live in.
	fileIDIndices map[fileID]int

	checksumToLocation checksumShardLocationMap

	// Indexed the same as recoverySet.
	fileIntegrityInfos []fileIntegrityInfo

	// parityLocations[i] is where recovery block i's data lives on disk;
	// blocks are read back on demand instead of being retained. Entries
	// with an empty path are absent — parityPresent is the source of truth.
	parityLocations []parityLocation
	// parityPresent[i] records that parity shard i exists, even when its
	// bytes were not retained.
	parityPresent []bool
}

// DecoderDelegate holds methods that are called during the decode
// process. Callbacks may arrive from concurrent phases (the data scan and
// the parity load overlap); implementations must be safe for concurrent use.
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
	_, hits, misses := scanBuffer(sliceByteCount, data, len(data), 0, checksumToLocation, fileID, fileIntegrityInfos, fileIDIndices, policy, new(sync.Mutex))
	return hits, misses
}

// scanBuffer scans buf for known shards, treating baseOffset as the file
// offset of buf[0]. It keeps scanning while the cursor is below limit, so a
// caller streaming a file can set limit short of the end and carry the
// remainder into the next window. It returns the cursor position where it
// stopped, plus hit and miss counts.
func scanBuffer(sliceByteCount int, data []byte, limit, baseOffset int, checksumToLocation checksumShardLocationMap, fileID fileID, fileIntegrityInfos []fileIntegrityInfo, fileIDIndices map[fileID]int, policy scanPolicy, mu *sync.Mutex) (int, int, int) {
	hits := 0
	misses := 0

	justMissed := false
	// The rolling window is only needed by the misaligned search, and is
	// fetched from the per-size cache the first time a miss slides.
	var window *crc32Window
	var crcSlice uint32
	// slideStart is the slice boundary the current byte-wise search began
	// from. Only meaningful while justMissed is true.
	slideStart := 0
	j := 0
	for j < limit {
		slice := sliceAndPadByteArray(data, j, j+sliceByteCount)
		if justMissed {
			if window == nil {
				window = crc32WindowFor(sliceByteCount)
			}
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
		// Guarded: another scanner may concurrently record a match into
		// this file's fileIntegrityInfo (duplicate slices live anywhere).
		d.scanMu.Lock()
		fileIntegrityInfos[i].missing = true
		d.scanMu.Unlock()
		return 0, 0, 0, nil
	} else if err != nil {
		return 0, 0, 0, err
	}
	defer closeFn()

	windowSize := scanWindowSize(d.sliceByteCount)
	buf := make([]byte, windowSize)

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
			checksumToLocation, info.fileID, fileIntegrityInfos, fileIDIndices, d.scanPolicy, &d.scanMu)
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

	sixteenK := md5.Sum(head)
	hasWrongByteCount := int(size) != info.byteCount

	// The file's verdict comes from its slices: every slice was CRC32- and
	// MD5-matched against the IFSC packet during the scan, so a file whose
	// slices all sit at their canonical offsets is byte-for-byte the
	// original and a second, whole-file MD5 pass over the same bytes would
	// only confirm what the slice hashes already proved. Skipping it halves
	// the hashing cost of a verify. (par2cmdline computes both; the two
	// verdicts differ only when the IFSC packet disagrees with the file
	// description packet, which no well-formed set does.)
	//
	// The flag writes share struct elements with concurrent match recording,
	// so they take the same lock; the delegate calls ride along so mid-scan
	// callbacks stay serialized.
	d.scanMu.Lock()
	hashMismatch := sixteenK != info.sixteenKHash ||
		!fileIntegrityInfos[i].allShardsOK(d.sliceByteCount)
	fileIntegrityInfos[i].hashMismatch = hashMismatch
	if hashMismatch {
		d.delegate.OnDetectDataFileHashMismatch(info.fileID, path)
	}
	fileIntegrityInfos[i].hasWrongByteCount = hasWrongByteCount
	if hasWrongByteCount {
		d.delegate.OnDetectDataFileWrongByteCount(info.fileID, path)
	}
	d.scanMu.Unlock()

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

	// Scan files on a worker pool: per-file hashing is independent and is
	// nearly all of the cost. Shard-match recording is guarded by scanMu
	// inside the scanner. Per-file delegate events are emitted afterwards in
	// file order, exactly as the sequential scan emitted them; on error,
	// every file is still scanned but the first erroring file in file order
	// wins, matching the sequential outcome for that file.
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

	if d.scanPolicy.findMisaligned {
		// The byte-wise search carries rolling state across windows, so
		// each file is scanned sequentially by one worker.
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
	} else if err := d.loadFileDataWindowed(checksumToLocation, fileIntegrityInfos, fileIDIndices, func(i, byteCount, hits, misses int, err error) {
		results[i] = scanResult{byteCount, hits, misses, err}
	}); err != nil {
		return err
	}

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

// loadFileDataWindowed scans every data file in slice-aligned windows over
// one shared worker pool (see forEachWindow), then derives each file's
// verdict flags from its slices and reports per-file results through
// report, in the same shape the per-file scanner produces.
func (d *Decoder) loadFileDataWindowed(checksumToLocation checksumShardLocationMap, fileIntegrityInfos []fileIntegrityInfo, fileIDIndices map[fileID]int, report func(i, byteCount, hits, misses int, err error)) error {
	sources := make([]windowSource, len(d.recoverySet))
	openErrs := make([]error, len(d.recoverySet))
	var closers []func() error
	defer func() {
		for _, c := range closers {
			_ = c()
		}
	}()
	for i, info := range d.recoverySet {
		r, size, closeFn, err := d.fileIO.OpenRead(d.getFilePath(info))
		if os.IsNotExist(err) {
			fileIntegrityInfos[i].missing = true
			continue
		} else if err != nil {
			openErrs[i] = err
			continue
		}
		closers = append(closers, closeFn)
		sources[i] = windowSource{r, size}
	}

	counts, scanErrs := d.scanFilesWindowed(sources, checksumToLocation, fileIntegrityInfos, fileIDIndices)

	for i, info := range d.recoverySet {
		if openErrs[i] != nil {
			report(i, 0, 0, 0, openErrs[i])
			continue
		}
		if sources[i].r == nil {
			report(i, 0, 0, 0, nil)
			continue
		}
		size := int(sources[i].size)
		if scanErrs[i] != nil {
			report(i, size, int(counts[i].hits.Load()), int(counts[i].misses.Load()), scanErrs[i])
			continue
		}
		path := d.getFilePath(info)
		hasWrongByteCount := size != info.byteCount
		// The verdict comes from the slices, as in fillFileIntegrityInfos:
		// every slice was CRC32- and MD5-matched during the scan, so no
		// second whole-file pass is needed.
		hashMismatch := counts[i].sixteenK != info.sixteenKHash ||
			!fileIntegrityInfos[i].allShardsOK(d.sliceByteCount)
		fileIntegrityInfos[i].hashMismatch = hashMismatch
		if hashMismatch {
			d.delegate.OnDetectDataFileHashMismatch(info.fileID, path)
		}
		fileIntegrityInfos[i].hasWrongByteCount = hasWrongByteCount
		if hasWrongByteCount {
			d.delegate.OnDetectDataFileWrongByteCount(info.fileID, path)
		}
		report(i, size, int(counts[i].hits.Load()), int(counts[i].misses.Load()), nil)
	}
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
//
// Recovery packets are taken on their validated headers and exponents
// alone: their bodies, which are the bulk of every volume, are not read or
// hash-checked here. LoadParityData, which repair uses, checks every packet
// in full before any block is used, so a corrupt block can never enter a
// repair; the trade is that a verify counts such a block as present.
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

	// Every packet of every volume is read and hash-checked on a shared
	// worker pool: on a large set the volumes hold hundreds of megabytes
	// of recovery data whose MD5 is the whole cost of this phase, and it
	// used to run on one goroutine while the data scan had the other
	// cores. Bookkeeping happens under one lock; delegate events are
	// recorded and replayed in (volume, packet) order afterwards, so the
	// observable sequence is the sequential one.
	type volume struct {
		path    string
		r       io.ReaderAt
		closeFn func() error
		refs    []packetRef
		err     error
	}
	vols := make([]*volume, len(matches))
	for i, match := range matches {
		v := &volume{path: match}
		vols[i] = v
		r, size, closeFn, err := d.fileIO.OpenRead(match)
		if err != nil {
			v.err = err
			continue
		}
		v.r, v.closeFn = r, closeFn
		v.refs, v.err = indexPackets(r, size)
	}
	defer func() {
		for _, v := range vols {
			if v.closeFn != nil {
				_ = v.closeFn()
			}
		}
	}()

	type event struct {
		vol, pkt int
		fn       func()
	}
	var (
		mu              sync.Mutex
		events          []event
		parityPresent   []bool
		parityLocations []parityLocation
		foundPacket     = make([]bool, len(vols))
		volMainPackets  = make([]*mainPacket, len(vols))
	)
	delegate := recoveryDelegate{d.delegate}
	record := func(vol, pkt int, fn func()) { events = append(events, event{vol, pkt, fn}) }

	// handle does the bookkeeping for one validated packet; called under mu.
	// For a recovery packet whose body was not read (presence mode), body
	// holds just the 4-byte exponent and dataLen gives the body's length.
	handle := func(vi, pi int, v *volume, setID recoverySetID, typ packetType, body []byte, packetOffset int64, dataLen int) error {
		if setID != d.setID {
			record(vi, pi, func() { delegate.OnOtherPacketSkip(setID, typ, len(body)) })
			return nil
		}
		foundPacket[vi] = true
		switch typ {
		case mainPacketType:
			mp, err := readMainPacket(body)
			if err != nil {
				return err
			}
			volMainPackets[vi] = &mp
			record(vi, pi, func() { delegate.OnMainPacketLoad(mp.sliceByteCount, len(mp.recoverySet), len(mp.nonRecoverySet)) })
		case recoveryPacketType:
			exp, _, err := readRecoveryPacket(body)
			if err != nil {
				return err
			}
			n := dataLen
			record(vi, pi, func() { delegate.OnRecoveryPacketLoad(uint16(exp), n) })
			if int(exp) >= len(parityPresent) {
				grow := int(exp+1) - len(parityPresent)
				parityPresent = append(parityPresent, make([]bool, grow)...)
				parityLocations = append(parityLocations, make([]parityLocation, grow)...)
			}
			parityPresent[exp] = true
			// Record where the block lives rather than copying it; the
			// fold reads it back per chunk pass. A recovery packet's body
			// is a 4-byte exponent then the data. The first volume in
			// match order wins for duplicate exponents (duplicates are
			// byte-identical by packet hash), regardless of which worker
			// got there first.
			loc := parityLocation{
				path:   v.path,
				offset: packetOffset + int64(sizeOfPacketHeader()) + 4,
				length: n,
			}
			if retain && (parityLocations[exp].path == "" || parityLocations[exp].vol > vi) {
				loc.vol = vi
				parityLocations[exp] = loc
			}
		case creatorPacketType:
			id := readCreatorPacket(body)
			record(vi, pi, func() { delegate.OnCreatorPacketLoad(id) })
		case fileDescriptionPacketType:
			// Parsed for validation only, matching readFile; the recovery
			// delegate suppresses the callback.
			fileID, fdp, err := readFileDescriptionPacket(body)
			if err != nil {
				return err
			}
			record(vi, pi, func() { delegate.OnFileDescriptionPacketLoad(fileID, fdp.filename, fdp.byteCount) })
		case ifscPacketType:
			fileID, _, err := readIFSCPacket(body)
			if err != nil {
				return err
			}
			record(vi, pi, func() { delegate.OnIFSCPacketLoad(fileID) })
		default:
			n := len(body)
			record(vi, pi, func() { delegate.OnUnknownPacketLoad(typ, n) })
		}
		return nil
	}

	type task struct{ vi, pi int }
	tasks := make(chan task)
	var wg sync.WaitGroup
	workers := max(d.numGoroutines, 1)
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			var buf []byte
			for t := range tasks {
				v := vols[t.vi]
				ref := v.refs[t.pi]
				var (
					setID   recoverySetID
					typ     packetType
					body    []byte
					dataLen int
					err     error
				)
				if !retain && packetType(ref.header.Type) == recoveryPacketType && ref.header.RecoverySetID == d.setID {
					// Presence only: the exponent is enough, the body
					// stays on disk unread (see LoadParityPresence).
					setID, typ = ref.header.RecoverySetID, recoveryPacketType
					body, dataLen, err = readRecoveryExponent(v.r, ref)
				} else {
					setID, typ, body, err = readPacketAt(v.r, ref, &buf)
					if typ == recoveryPacketType {
						dataLen = len(body) - 4
					}
				}
				mu.Lock()
				if v.err == nil {
					if err != nil {
						v.err = err
					} else {
						v.err = handle(t.vi, t.pi, v, setID, typ, body, ref.offset, dataLen)
					}
				}
				mu.Unlock()
			}
		}()
	}
	for vi, v := range vols {
		if v.err != nil {
			continue
		}
		for pi := range v.refs {
			tasks <- task{vi, pi}
		}
	}
	close(tasks)
	wg.Wait()

	sort.Slice(events, func(a, b int) bool {
		if events[a].vol != events[b].vol {
			return events[a].vol < events[b].vol
		}
		return events[a].pkt < events[b].pkt
	})
	for _, e := range events {
		e.fn()
	}

	for i, v := range vols {
		volumeErr := v.err
		if volumeErr == nil && foundPacket[i] {
			if mp := volMainPackets[i]; mp != nil {
				switch {
				case d.sliceByteCount != mp.sliceByteCount:
					volumeErr = errors.New("slice byte count mismatch")
				case !reflect.DeepEqual(decoderInputFileInfoIDs(d.recoverySet), mp.recoverySet):
					volumeErr = errors.New("recovery set mismatch")
				case !reflect.DeepEqual(decoderInputFileInfoIDs(d.nonRecoverySet), mp.nonRecoverySet):
					volumeErr = errors.New("non-recovery set mismatch")
				}
			}
		}
		d.delegate.OnParityFileLoad(i+1, v.path, volumeErr)
		if volumeErr != nil {
			return volumeErr
		}
	}

	d.parityPresent = parityPresent
	d.parityLocations = parityLocations
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
//
// Repaired files are written to temporary siblings and renamed into place,
// so a failed repair leaves the originals untouched.
func (d *Decoder) Repair(checkParity bool) ([]string, error) {
	wasOK := make([]bool, len(d.fileIntegrityInfos))
	for i, info := range d.fileIntegrityInfos {
		wasOK[i] = info.ok(d.sliceByteCount)
	}

	cache := newReaderCache(d)
	defer cache.Close()

	// Reading shards on demand means rewriting one file can destroy the
	// source of another's shards (two files' contents swapped). Pre-read any
	// cross-file surviving shard whose source file is being rewritten, before
	// any file is opened for writing.
	preserved := make(map[int][]byte)
	if err := d.preserveShardsBeforeOverwrite(cache, preserved, wasOK); err != nil {
		return nil, err
	}

	// shardBase[i] is file i's first global shard row.
	shardBase := make([]int, len(d.recoverySet))
	base := 0
	for i, info := range d.fileIntegrityInfos {
		shardBase[i] = base
		base += len(info.shardInfos)
	}

	// Open a positional writer per file needing repair and copy its surviving
	// shards to their canonical offsets; reconstructed ranges stream in
	// afterwards, so no whole file is ever buffered.
	type openFile struct {
		w       io.WriterAt
		closeFn func(commit bool) error
		// written lists the file's shard indices whose bytes this repair
		// wrote; nil means the whole file was written (no clone), and
		// the post-write check then covers every slice.
		written []int
		cloned  bool
	}
	writers := make([]*openFile, len(d.recoverySet))
	rowToFile := make(map[int]int)
	for i, info := range d.fileIntegrityInfos {
		if wasOK[i] {
			continue
		}
		for j, si := range info.shardInfos {
			if !si.present {
				rowToFile[shardBase[i]+j] = i
			}
		}
	}
	// Discard every still-open writer: an aborted repair must leave the
	// originals untouched.
	closeAll := func() {
		for _, of := range writers {
			if of != nil && of.closeFn != nil {
				_ = of.closeFn(false)
				of.closeFn = nil
			}
		}
	}

	// Files are independent here, so each rewritten file's survivor copy
	// runs on its own goroutine, bounded by numGoroutines: the copy is a
	// read+write of nearly the whole file and was a serial pass before.
	// preserved and the plan are read-only; the reader cache is
	// concurrency-safe.
	workers := max(d.numGoroutines, 1)
	copyErrs := make([]error, len(d.recoverySet))
	{
		sem := make(chan struct{}, workers)
		var wg sync.WaitGroup
		for i, inputFileInfo := range d.recoverySet {
			if wasOK[i] {
				continue
			}
			wg.Add(1)
			sem <- struct{}{}
			go func(i int, inputFileInfo decoderInputFileInfo) {
				defer wg.Done()
				defer func() { <-sem }()
				copyErrs[i] = d.copySurvivingShards(cache, preserved, shardBase[i], i, inputFileInfo, func(of io.WriterAt, closeFn func(bool) error, cloned bool, written []int) {
					writers[i] = &openFile{w: of, closeFn: closeFn, cloned: cloned, written: written}
				})
			}(i, inputFileInfo)
		}
		wg.Wait()
	}
	for _, err := range copyErrs {
		if err != nil {
			closeAll()
			return nil, err
		}
	}

	// Stream reconstruction straight into the open files.
	_, err := d.reconstructTo(cache, preserved, func(row, off int, data []byte) error {
		i, ok := rowToFile[row]
		if !ok {
			return errors.New("reconstructed a shard no file was waiting for")
		}
		fileOff := (row-shardBase[i])*d.sliceByteCount + off
		if off == 0 && writers[i].cloned {
			writers[i].written = append(writers[i].written, row-shardBase[i])
		}
		return writeShardRange(writers[i].w, fileOff, data, d.recoverySet[i].byteCount)
	})
	if err != nil {
		closeAll()
		return nil, err
	}

	// Release every reader before committing: Windows refuses to rename over
	// a file that is still open, and the cache holds the originals open for
	// survivor reads. Nothing below needs it — checkWrittenFile opens the
	// repaired file itself, and the parity check builds its own cache.
	cache.Close()

	// Commit every rewritten file (close plus rename, run across files in
	// parallel), then verify what was written by streaming it back. A file
	// rewritten from a clone had only its reconstructed and relocated
	// slices written, and only those are re-read and hashed, since the
	// rest share their blocks with the original the scan already verified;
	// a file written from scratch is checked in full. checkParity (the
	// caller's DoubleCheck) checks every file in full regardless. Results
	// are reported in file order so delegate events and the error outcome
	// match the sequential behaviour.
	var repairedPaths []string
	commitErrs := make([]error, len(d.recoverySet))
	{
		sem := make(chan struct{}, workers)
		var wg sync.WaitGroup
		for i := range d.recoverySet {
			of := writers[i]
			if of == nil {
				continue
			}
			wg.Add(1)
			sem <- struct{}{}
			go func(i int, of *openFile) {
				defer wg.Done()
				defer func() { <-sem }()
				commitErrs[i] = of.closeFn(true)
				of.closeFn = nil
			}(i, of)
		}
		wg.Wait()
	}
	for i := range d.recoverySet {
		if commitErrs[i] != nil {
			return repairedPaths, commitErrs[i]
		}
	}
	checkPaths := make([]string, len(d.recoverySet))
	checkRows := make([][]int, len(d.recoverySet))
	for i, inputFileInfo := range d.recoverySet {
		if writers[i] == nil {
			continue
		}
		checkPaths[i] = d.getFilePath(inputFileInfo)
		if writers[i].cloned && !checkParity {
			checkRows[i] = writers[i].written
		}
	}
	checkErrs := d.checkWrittenFiles(checkPaths, checkRows)
	for i, inputFileInfo := range d.recoverySet {
		if writers[i] == nil {
			continue
		}
		path := d.getFilePath(inputFileInfo)
		err := checkErrs[i]
		d.delegate.OnDataFileWrite(i+1, len(d.recoverySet), path, inputFileInfo.byteCount, err)
		if err != nil {
			return repairedPaths, err
		}
		repairedPaths = append(repairedPaths, path)

		// The file is now whole: every shard sits at its canonical offset.
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
		// parity double-check streams from the repaired files. It needs a
		// fresh cache: the one above holds readers opened before the
		// rewrites, which would serve pre-repair content.
		verifyCache := newReaderCache(d)
		defer verifyCache.Close()
		if err := d.verifyParity(verifyCache); err != nil {
			return repairedPaths, err
		}
	}

	// TODO: Repair missing parity volumes, too, and then make
	// sure d.Verify() passes.

	return repairedPaths, nil
}

// copySurvivingShards opens file i for rewriting, hands the writer to
// register, and copies every surviving shard of the file to its canonical
// offset. Missing shards are left for reconstruction to stream in.
//
// When the fileIO can clone the damaged original (patchFileIO), the rewrite
// starts from that clone and shards already sitting at their canonical
// offset in this very file are left alone: their bytes are the ones the
// scan verified. Only relocated shards (found elsewhere in the set) are
// copied, and register receives their indices so the post-write check can
// cover exactly what was written.
func (d *Decoder) copySurvivingShards(cache *readerCache, preserved map[int][]byte, shardBase, i int, inputFileInfo decoderInputFileInfo, register func(w io.WriterAt, closeFn func(bool) error, cloned bool, written []int)) error {
	path := d.getFilePath(inputFileInfo)
	info := d.fileIntegrityInfos[i]

	var (
		w       io.WriterAt
		closeFn func(bool) error
		err     error
		cloned  bool
	)
	if pio, ok := d.fileIO.(patchFileIO); ok && !info.missing {
		w, closeFn, err = pio.OpenPatch(path, int64(inputFileInfo.byteCount))
		cloned = err == nil
	}
	if !cloned {
		w, closeFn, err = d.fileIO.OpenWrite(path, int64(inputFileInfo.byteCount))
		if err != nil {
			return err
		}
	}

	var written []int
	shardBuf := make([]byte, d.sliceByteCount)
	for j, si := range info.shardInfos {
		if !si.present {
			continue
		}
		canonical := shardLocation{info.fileID, j * d.sliceByteCount}
		_, isPreserved := preserved[shardBase+j]
		if cloned && !isPreserved && si.foundAt == canonical {
			continue
		}
		if shard, ok := preserved[shardBase+j]; ok {
			clear(shardBuf)
			copy(shardBuf, shard)
		} else if err := cache.readRange(si, 0, shardBuf); err != nil {
			_ = closeFn(false)
			return err
		}
		if err := writeShardRange(w, j*d.sliceByteCount, shardBuf, inputFileInfo.byteCount); err != nil {
			_ = closeFn(false)
			return err
		}
		if cloned {
			written = append(written, j)
		}
	}
	register(w, closeFn, cloned, written)
	return nil
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
	if n <= 0 {
		return nil
	}
	_, err := w.WriteAt(data[:n], int64(fileOff))
	return err
}

// checkWrittenFile streams a repaired file back and verifies its 16k hash and
// MD5, the same windowed read the scan uses.
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

// preserveShardsBeforeOverwrite reads, into preserved, every surviving shard
// that a file needing repair sources from another file that is also about to
// be rewritten.
func (d *Decoder) preserveShardsBeforeOverwrite(cache *readerCache, preserved map[int][]byte, wasOK []bool) error {
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
			if _, done := preserved[idx]; done {
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
			preserved[idx] = buf
		}
		shardBase += len(info.shardInfos)
	}
	return nil
}

// verifyParity recomputes the recovery blocks from the repaired data and
// compares them against the ones on disk. It folds the data shards in the same
// bounded way reconstruction does, so the double check does not undo the
// memory saving it is checking.
func (d *Decoder) verifyParity(cache *readerCache) error {
	coder, _, _, _, _, err := d.planReconstruction()
	if err != nil {
		return err
	}

	var dataShards []shardIntegrityInfo
	for _, info := range d.fileIntegrityInfos {
		dataShards = append(dataShards, info.shardInfos...)
	}

	parityCount := len(d.parityPresent)
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

		// Inputs load concurrently, so each call reads into its own
		// pooled slice buffer rather than one shared scratch.
		bufs := sync.Pool{New: func() any { b := make([]byte, d.sliceByteCount); return &b }}
		err := rsec16.FoldInputsParallel(coder.ParityMatrix(), len(dataShards), n, func(j int, dst []byte) error {
			bp := bufs.Get().(*[]byte)
			defer bufs.Put(bp)
			buf := *bp
			if err := cache.readRange(dataShards[j], 0, buf); err != nil {
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

		// Compare against the stored recovery blocks, read back from
		// their volumes a chunk at a time.
		stored := make([]byte, n)
		for i, loc := range d.parityLocations {
			if loc.path == "" || off >= loc.length {
				continue
			}
			end := off + n
			if end > loc.length {
				end = loc.length
			}
			if err := cache.readAbs(loc.path, loc.offset+int64(off), stored[:end-off]); err != nil {
				return err
			}
			if !bytes.Equal(out[i][:end-off], stored[:end-off]) {
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
