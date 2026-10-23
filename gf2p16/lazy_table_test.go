package gf2p16

import (
	"sync"
	"testing"
)

// The scalar field operations do not use region tables and provide an
// independent oracle while many callers initialize the same coefficients.
func TestConcurrentRegionTableInitialization(t *testing.T) {
	var wg sync.WaitGroup
	for worker := 0; worker < 32; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			c := T(61000 + worker%4)
			in, out := make([]T, 257), make([]T, 257)
			for i := range in {
				in[i] = T(i*239 + worker)
			}
			mulSliceGeneric(c, in, out)
			for i := range in {
				if out[i] != c.Times(in[i]) {
					t.Errorf("coefficient %d, input %d: incorrect product", c, in[i])
					return
				}
			}
		}(worker)
	}
	wg.Wait()
}
