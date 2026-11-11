package rsec16

import "testing"

func TestGoroutineCount(t *testing.T) {
	cases := []struct{ procs, physical, logical, want int }{
		{10, 10, 10, 10},
		{8, 4, 8, 4},  // hyperthreads: prefer physical cores
		{4, 8, 8, 4},  // GOMAXPROCS below core count
		{8, 0, 8, 8},  // physical core count unknown (Rosetta, VMs)
		{8, 0, 0, 8},  // nothing known but GOMAXPROCS
		{0, 0, 0, 1},  // never below one
		{4, -1, 2, 2}, // bogus physical count falls back to logical
		{1, 0, 16, 1},
	}
	for _, c := range cases {
		if got := goroutineCount(c.procs, c.physical, c.logical); got != c.want {
			t.Errorf("goroutineCount(%d,%d,%d) = %d, want %d", c.procs, c.physical, c.logical, got, c.want)
		}
	}
	if DefaultNumGoroutines() < 1 {
		t.Error("DefaultNumGoroutines() < 1")
	}
}
