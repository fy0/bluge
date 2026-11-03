package bluge

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fy0/bluge/index"
	"github.com/fy0/bluge/index/mergeplan"
)

var writerMemoryCase = flag.String("writer-memory-case", "", "opt-in process memory experiment: stream or parallel")
var writerMemoryProfile = flag.String("writer-memory-profile", "default", "default, balanced, small, edge, edge-burst, or edge-single")
var writerMemoryLazy = flag.Bool("writer-memory-lazy", false, "build parallel batches only after admission")
var writerMemoryProducers = flag.Int("writer-memory-producers", 8, "parallel batch producers")
var writerMemoryMiB = flag.Uint64("writer-memory-mib", 32, "input MiB (plus a quarter-size replacement for stream)")
var writerMemoryDense = flag.Bool("writer-memory-dense", false, "use dense short tokens and term positions")
var writerMemoryMergeMiB = flag.Int64("writer-memory-merge-mib", 4, "maximum segment MiB in byte-planned profiles")

func experimentInputBytes(mib uint64) (int64, error) {
	if mib == 0 || mib > math.MaxInt64/(1<<20+1<<18) {
		return 0, fmt.Errorf("input MiB must be positive and fit in an int64 with its replacement")
	}
	return int64(mib) << 20, nil
}

// Run each experiment in a fresh test process, with -test.run restricted to this
// test. OS peak RSS includes mappings and runtime overhead; sampled HeapAlloc
// and cumulative TotalAlloc describe different quantities, not a memory limit.
func TestWriterMemoryExperiment(t *testing.T) {
	if *writerMemoryCase == "" {
		t.Skip("set -writer-memory-case to run")
	}
	mib := *writerMemoryMiB
	inputBytes, err := experimentInputBytes(mib)
	if err != nil {
		t.Fatal(err)
	}
	if *writerMemoryProducers < 1 {
		t.Fatal("producers must be positive")
	}
	if *writerMemoryMergeMiB < 1 || *writerMemoryMergeMiB > 1024 {
		t.Fatal("merge MiB must be between 1 and 1024")
	}
	bytePlanning := strings.HasPrefix(*writerMemoryProfile, "edge")
	path := filepath.Join(t.TempDir(), "index")
	cfg := DefaultConfig(path)
	switch *writerMemoryProfile {
	case "default":
	case "balanced", "small", "edge", "edge-single", "edge-burst":
		workers := 2
		if *writerMemoryProfile == "small" || *writerMemoryProfile == "edge-single" {
			workers = 1
		}
		options := mergeplan.DefaultMergePlanOptions
		options.SegmentsPerMergeTask = 4
		if bytePlanning {
			options.MaxSegmentSize = *writerMemoryMergeMiB * 1024 * 1024
			options.FloorSegmentSize = 256 * 1024
			// The experiment is also copied into the baseline checkout. Only
			// candidate profiles require the two new APIs.
			planner, ok := any(cfg).(interface{ WithMergePlanningByBytes(bool) Config })
			if !ok {
				t.Fatal("byte planning unavailable in this revision")
			}
			cfg = planner.WithMergePlanningByBytes(true)
		}
		slots := workers
		if *writerMemoryProfile == "edge-burst" {
			slots = 4
		}
		cfg = cfg.WithNumAnalysisWorkers(workers).WithMaxConcurrentBatches(slots).
			WithMaxBatchInputBytes(512 * 1024).WithMergeBufferSize(64 * 1024).WithMergePlanOptions(options)
	default:
		t.Fatal("unknown profile")
	}
	var text strings.Builder
	for i := 0; text.Len() < 64*1024; i++ {
		if *writerMemoryDense {
			text.WriteString("red fox dog cat sun sky common ")
		} else {
			fmt.Fprintf(&text, "common device document token%04x search storage measurement. ", i%4096)
		}
	}
	pattern := []byte(text.String())
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	stop, sampled := make(chan struct{}), make(chan uint64, 1)
	go func() {
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		var peak uint64
		for {
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			if m.HeapAlloc > peak {
				peak = m.HeapAlloc
			}
			select {
			case <-stop:
				sampled <- peak
				return
			case <-ticker.C:
			}
		}
	}()
	defer close(stop)
	started := time.Now()
	w, err := OpenWriter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	var expected uint64
	switch *writerMemoryCase {
	case "stream":
		opts := TextChunkOptions{MaxChunkBytes: 64 * 1024, BatchSize: 4}
		result, err := w.ImportText("source", "body", &memoryExperimentReader{pattern: pattern, remaining: inputBytes}, opts)
		if err != nil || result.CommittedEndByte != inputBytes {
			t.Fatalf("initial import: %+v %v", result, err)
		}
		pinned, err := w.Reader()
		if err != nil {
			t.Fatal(err)
		}
		pinnedClosed := false
		defer func() {
			if !pinnedClosed {
				_ = pinned.Close()
			}
		}()
		oldCount := result.CommittedChunks
		result, err = w.ImportText("source", "body", &memoryExperimentReader{pattern: pattern, remaining: inputBytes / 4}, opts)
		if err != nil || result.CommittedEndByte != inputBytes/4 {
			t.Fatalf("replacement: %+v %v", result, err)
		}
		if count, err := pinned.Count(); err != nil || count != oldCount {
			t.Fatalf("pinned snapshot: %d %v", count, err)
		}
		expected = result.CommittedChunks
		inputBytes += inputBytes / 4
		if err := pinned.Close(); err != nil {
			t.Fatal(err)
		}
		pinnedClosed = true
	case "parallel":
		var submit func(context.Context, func(*index.Batch) error) error
		if *writerMemoryLazy {
			builder, ok := any(w).(interface {
				BatchFunc(context.Context, func(*index.Batch) error) error
			})
			if !ok {
				t.Fatal("deferred construction unavailable in this revision")
			}
			submit = builder.BatchFunc
		}
		const docsPerBatch, docBytes = 64, 4096
		batches := mib * (1 << 20) / (docsPerBatch * docBytes)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		jobs := make(chan uint64)
		errors := make(chan error, *writerMemoryProducers)
		var wg sync.WaitGroup
		for worker := 0; worker < *writerMemoryProducers; worker++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for n := range jobs {
					build := func(b *index.Batch) error {
						for j := 0; j < docsPerBatch; j++ {
							body := append([]byte(nil), pattern[:docBytes]...)
							b.Insert(NewDocument(fmt.Sprintf("doc-%d-%d", n, j)).AddField(NewTextFieldBytes("body", body).StoreValue().SearchTermPositions()))
						}
						return nil
					}
					var err error
					if submit != nil {
						err = submit(ctx, build)
					} else {
						b := NewBatch()
						err = build(b)
						if err == nil {
							err = w.Batch(b)
						}
					}
					if err != nil {
						errors <- err
						cancel()
						return
					}
				}
			}()
		}
		go func() {
			defer close(jobs)
			for n := range batches {
				select {
				case jobs <- n:
				case <-ctx.Done():
					return
				}
			}
		}()
		wg.Wait()
		close(errors)
		for err := range errors {
			t.Fatal(err)
		}
		expected = batches * docsPerBatch
	default:
		t.Fatal("unknown case")
	}
	writeMS := float64(time.Since(started).Microseconds()) / 1000
	closeStart := time.Now()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	closeMS := float64(time.Since(closeStart).Microseconds()) / 1000
	r, err := OpenReader(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if count, err := r.Count(); err != nil || count != expected {
		t.Fatalf("reopened count: %d, want %d, error %v", count, expected, err)
	}
	latencies := make([]float64, 100)
	for i := range latencies {
		start := storageBenchNow()
		it, err := r.Search(context.Background(), NewTopNSearch(10, NewTermQuery("common").SetField("body")))
		if err != nil {
			t.Fatal(err)
		}
		hits := 0
		for {
			match, err := it.Next()
			if err != nil {
				t.Fatal(err)
			}
			if match == nil {
				break
			}
			hits++
		}
		maxHits := int(min(expected, 10))
		minHits := maxHits
		// A short final text chunk may end before its first "common" token.
		if *writerMemoryCase == "stream" && expected <= 10 && minHits > 0 {
			minHits--
		}
		if hits < minHits || hits > maxHits {
			t.Fatalf("query hits: %d", hits)
		}
		latencies[i] = storageBenchElapsedMS(start)
	}
	sort.Float64s(latencies)
	runtime.ReadMemStats(&after)
	// A signal stops sampling normally; the deferred close also handles failures.
	stop <- struct{}{}
	peakHeap := <-sampled
	stats := w.chill.Stats() // Writer is closed; no concurrent stats mutation.
	peakRSS, rssErr := writerMemoryPeakRSS()
	if rssErr != nil {
		t.Fatal(rssErr)
	}
	report := map[string]interface{}{
		"case": *writerMemoryCase, "profile": *writerMemoryProfile, "dense": *writerMemoryDense, "lazy": *writerMemoryLazy,
		"producers": *writerMemoryProducers, "workers": cfg.indexConfig.NumAnalysisWorkers, "batch_slots": cfg.maxConcurrentBatches,
		"byte_planning": bytePlanning, "max_segment_size": cfg.indexConfig.MergePlanOptions.MaxSegmentSize,
		"go": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH, "gomaxprocs": runtime.GOMAXPROCS(0),
		"input_bytes": inputBytes, "documents": expected, "write_ms": writeMS, "close_ms": closeMS,
		"total_ms": float64(time.Since(started).Microseconds()) / 1000, "query_p50_ms": latencies[50], "query_p95_ms": latencies[95],
		"peak_rss_bytes": peakRSS, "sampled_peak_heap_bytes": peakHeap, "allocated_bytes": after.TotalAlloc - before.TotalAlloc,
		"gc_cycles": after.NumGC - before.NumGC, "disk_bytes": stats.CurOnDiskBytes, "disk_files": stats.CurOnDiskFiles,
		"disk_merges": stats.TotFileMergeZapEnd, "memory_merges": stats.TotMemMergeZapEnd,
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("WRITER_MEMORY_RESULT %s\n", encoded)
}

type memoryExperimentReader struct {
	pattern   []byte
	offset    int
	remaining int64
}

func (r *memoryExperimentReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n := 0
	for n < len(p) {
		copied := copy(p[n:], r.pattern[r.offset:])
		n += copied
		r.offset = (r.offset + copied) % len(r.pattern)
	}
	r.remaining -= int64(n)
	return n, nil
}
