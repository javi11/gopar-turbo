package par2

// VerifyDelegate is just DecoderDelegate for now.
type VerifyDelegate interface {
	DecoderDelegate
}

// DoNothingVerifyDelegate is an implementation of VerifyDelegate that
// does nothing for all methods.
type DoNothingVerifyDelegate struct {
	DoNothingDecoderDelegate
}

// VerifyOptions holds all the options for Verify.
type VerifyOptions struct {
	// The number of goroutines to use while encoding. If <= 0,
	// NumGoroutinesDefault() is used.
	NumGoroutines int
	// The VerifyDelegate to use. If nil, DoNothingVerifyDelegate
	// is used.
	VerifyDelegate VerifyDelegate
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

// VerifyResult holds the result of a Verify call.
type VerifyResult struct {
	// ShardCounts contains shard counts which can be used to deduce
	// whether repair is necessary and/or possible.
	ShardCounts ShardCounts
}

// Verify a par file at parPath with the given options. The returned
// VerifyResult is not filled in if an error is returned.
func Verify(parPath string, options VerifyOptions) (VerifyResult, error) {
	return verify(defaultFileIO{}, parPath, options)
}

func verify(fileIO fileIO, parPath string, options VerifyOptions) (VerifyResult, error) {
	err := checkExtension(parPath)
	if err != nil {
		return VerifyResult{}, err
	}

	delegate := options.VerifyDelegate
	if delegate == nil {
		delegate = DoNothingVerifyDelegate{}
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
		return VerifyResult{}, err
	}

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

	return VerifyResult{
		ShardCounts: decoder.ShardCounts(),
	}, nil
}
