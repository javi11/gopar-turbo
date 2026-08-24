package par2

// fallbackMemoryBudget is used when physical memory cannot be determined.
const fallbackMemoryBudget = 1 << 30

// defaultMemoryBudget returns the bytes repair may hold for reconstruction
// accumulators when the caller does not specify a budget. It mirrors
// par2cmdline's default of half of physical memory.
func defaultMemoryBudget() int {
	total := physicalMemory()
	if total <= 0 {
		return fallbackMemoryBudget
	}
	half := total / 2
	if half > int64(^uint(0)>>1) {
		return int(^uint(0) >> 1)
	}
	return int(half)
}
