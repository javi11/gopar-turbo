package par2

import (
	"github.com/javi11/gopar-turbo/rsec16"
)

// RepairDelegate is just DecoderDelegate for now.
type RepairDelegate interface {
	DecoderDelegate
}

// DoNothingRepairDelegate is an implementation of RepairDelegate that
// does nothing for all methods.
type DoNothingRepairDelegate struct {
	DoNothingDecoderDelegate
}

// RepairOptions holds all the options for Repair.
type RepairOptions struct {
	// If DoubleCheck is true, then extra checking is done after
	// the repair to verify that the repaired shards are correct.
	DoubleCheck bool
	// The number of goroutines to use while encoding. If <= 0,
	// NumGoroutinesDefault() is used.
	NumGoroutines int
	// The RepairDelegate to use. If nil, DoNothingRepairDelegate
	// is used.
	RepairDelegate RepairDelegate
	// If FindMisalignedData is true, a slice that matches no known
	// shard triggers a byte-wise search for shards that sit off a
	// slice boundary, as when data has been shifted by an insertion
	// or deletion. This recovers sets that would otherwise need
	// parity, but costs up to a full slice of byte-wise scanning per
	// miss. par2cmdline exposes the same trade-off as -N.
	//
	// Defaults to false: a miss advances to the next slice boundary.
	FindMisalignedData bool
	// MisalignedSearchLimit bounds how far past a slice boundary the
	// FindMisalignedData search slides before giving up on that
	// slice. Zero means unbounded. Ignored when FindMisalignedData
	// is false.
	MisalignedSearchLimit int
	// MemoryBudget caps the bytes held for reconstruction accumulators.
	// Zero selects a default capped at 256 MB; chunked passes at that size
	// are measured to cost no wall-clock, so larger values buy nothing.
	// When the budget is smaller than one accumulator per missing shard,
	// repair splits slices into byte ranges and makes several passes over
	// the inputs rather than exceeding it.
	MemoryBudget int
}

// RepairResult holds the result of a Repair call.
type RepairResult struct {
	// RepairedPaths contains the paths of the files that were
	// repaired.
	RepairedPaths []string
}

// Repair a par file at parPath with the given options. The returned
// RepairResult may be partially or not filled in if an error is
// returned.
func Repair(parPath string, options RepairOptions) (RepairResult, error) {
	return repair(defaultFileIO{}, parPath, options)
}

func repair(fileIO fileIO, parPath string, options RepairOptions) (RepairResult, error) {
	err := checkExtension(parPath)
	if err != nil {
		return RepairResult{}, err
	}

	delegate := options.RepairDelegate
	if delegate == nil {
		delegate = DoNothingRepairDelegate{}
	}

	numGoroutines := options.NumGoroutines
	if numGoroutines <= 0 {
		numGoroutines = NumGoroutinesDefault()
	}

	decoder, err := newDecoder(fileIO, delegate, parPath, numGoroutines, scanPolicy{
		findMisaligned: options.FindMisalignedData,
		searchLimit:    options.MisalignedSearchLimit,
	}, options.MemoryBudget)
	if err != nil {
		return RepairResult{}, err
	}

	// The scan and the parity load touch disjoint files and disjoint
	// decoder fields; running them concurrently hides the shorter phase
	// entirely.
	scanErr := make(chan error, 1)
	go func() { scanErr <- decoder.LoadFileData() }()
	parityErr := decoder.LoadParityData()
	if err := <-scanErr; err != nil {
		return RepairResult{}, err
	}
	if parityErr != nil {
		return RepairResult{}, parityErr
	}

	repairedPaths, err := decoder.Repair(options.DoubleCheck)
	return RepairResult{
		RepairedPaths: repairedPaths,
	}, err
}

// RepairErrorMeansRepairNecessaryButNotPossible returns true if the
// error returned by Repair means that repair is necessary but not
// possible.
func RepairErrorMeansRepairNecessaryButNotPossible(err error) bool {
	_, ok := err.(rsec16.NotEnoughParityShardsError)
	return ok
}
