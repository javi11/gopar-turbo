// Command par2bench times a single gopar-turbo verify or repair operation
// against a PAR2 set, reporting wall time and peak RSS as JSON on stdout.
//
// It is a benchmark driver, not a supported CLI surface: one process per
// measured operation, so peak RSS is attributable to that operation alone.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"runtime"
	"runtime/pprof"
	"time"

	"github.com/javi11/gopar-turbo/gf16"
	"github.com/javi11/gopar-turbo/par2"
)

type result struct {
	Tool           string   `json:"tool"`
	Op             string   `json:"op"`
	Backend        string   `json:"backend"`
	Accelerated    bool     `json:"accelerated"`
	NumGoroutines  int      `json:"num_goroutines"`
	FindMisaligned bool     `json:"find_misaligned"`
	Seconds        float64  `json:"seconds"`
	PeakRSSBytes   int64    `json:"peak_rss_bytes"`
	RepairNeeded   bool     `json:"repair_needed"`
	RepairPossible bool     `json:"repair_possible"`
	RepairedPaths  []string `json:"repaired_paths,omitempty"`
	Err            string   `json:"error,omitempty"`
}

func main() {
	op := flag.String("op", "verify", "operation: verify | repair")
	parPath := flag.String("par", "", "path to the .par2 index file")
	goroutines := flag.Int("goroutines", 0, "NumGoroutines (0 = default)")
	doubleCheck := flag.Bool("double-check", false, "verify repaired shards after repair")
	findMisaligned := flag.Bool("find-misaligned", false, "search for shards that sit off a slice boundary (par2cmdline -N)")
	searchLimit := flag.Int("misaligned-limit", 0, "bound the misaligned search slide; 0 = unbounded")
	memProfile := flag.String("memprofile", "", "write an in-use heap profile to this path")
	cpuProfile := flag.String("cpuprofile", "", "write a CPU profile to this path")
	memoryBudget := flag.Int("memory-budget", 0, "MemoryBudget in bytes (0 = library default)")
	flag.Parse()

	if *parPath == "" {
		fmt.Fprintln(os.Stderr, "par2bench: -par is required")
		os.Exit(2)
	}

	r := result{
		Tool:           "gopar-turbo",
		Op:             *op,
		Accelerated:    gf16.Accelerated(),
		NumGoroutines:  *goroutines,
		FindMisaligned: *findMisaligned,
	}
	if r.Accelerated {
		r.Backend = "cgo/ParPar SIMD"
	} else {
		r.Backend = "pure-Go gf2p16"
	}
	if r.NumGoroutines == 0 {
		r.NumGoroutines = par2.NumGoroutinesDefault()
	}

	if *cpuProfile != "" {
		f, ferr := os.Create(*cpuProfile)
		if ferr == nil {
			_ = pprof.StartCPUProfile(f)
			defer pprof.StopCPUProfile()
		}
	}

	start := time.Now()
	var err error
	switch *op {
	case "verify":
		var vr par2.VerifyResult
		vr, err = par2.Verify(*parPath, par2.VerifyOptions{
			NumGoroutines:         *goroutines,
			FindMisalignedData:    *findMisaligned,
			MisalignedSearchLimit: *searchLimit,
		})
		if err == nil {
			r.RepairNeeded = vr.ShardCounts.RepairNeeded()
			r.RepairPossible = vr.ShardCounts.RepairPossible()
		}
	case "repair":
		var rr par2.RepairResult
		rr, err = par2.Repair(*parPath, par2.RepairOptions{
			NumGoroutines:         *goroutines,
			DoubleCheck:           *doubleCheck,
			FindMisalignedData:    *findMisaligned,
			MisalignedSearchLimit: *searchLimit,
			MemoryBudget:          *memoryBudget,
		})
		r.RepairedPaths = rr.RepairedPaths
	default:
		fmt.Fprintf(os.Stderr, "par2bench: unknown -op %q\n", *op)
		os.Exit(2)
	}
	r.Seconds = time.Since(start).Seconds()

	if *memProfile != "" {
		f, ferr := os.Create(*memProfile)
		if ferr == nil {
			_ = pprof.Lookup("heap").WriteTo(f, 0)
			f.Close()
		}
	}
	r.PeakRSSBytes = peakRSS()

	if err != nil {
		r.Err = err.Error()
	}

	// Keep the process alive to this point so peak RSS is measured before
	// the runtime can release anything back to the OS.
	runtime.KeepAlive(r)

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(r)

	if err != nil {
		os.Exit(1)
	}
}
