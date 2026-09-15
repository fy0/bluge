//go:build (windows || linux || darwin) && (amd64 || arm64)

package bluge

import (
	"fmt"
	"runtime"
	"sync"
	"testing"
)

// TestUsearchGateBoundsConcurrentSearches hammers a single segment handle
// with more searchers than the native context pool and checks that every
// call succeeds, that the in-flight count never exceeds the negotiated pool
// size, and that calls actually overlap instead of serializing.
func TestUsearchGateBoundsConcurrentSearches(t *testing.T) {
	corpus := smallSearchCorpus(t, 800, 4, 0)
	defer corpus.Close()
	query := benchQuery(16)

	index := corpus.embeddedIndex(t)
	segments := index.fields[embeddedBenchField]
	if len(segments) == 0 {
		t.Fatal("corpus has no embedded usearch segments")
	}
	api, ok := segments[0].api.(*puregoUSearchAPI)
	if !ok {
		t.Fatalf("expected the purego adapter, got %T", segments[0].api)
	}
	segment := segments[0]

	want := usearchSearchThreads
	if api.abiVersion < usearchABIVersionReserveThreads {
		// ABI 3 libraries cannot grow the context pool past the default
		// hardware_concurrency size.
		want = fallbackUsearchConcurrency()
	}
	gateValue, ok := api.handleGates.Load(segment.handle)
	if !ok {
		t.Fatal("segment handle has no concurrency gate")
	}
	gate := gateValue.(*usearchHandleGate)
	if cap(gate.slots) != want {
		t.Fatalf("gate capacity %d, want %d", cap(gate.slots), want)
	}

	workers := 2 * want
	start := make(chan struct{})
	var wg sync.WaitGroup
	failures := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			keys := make([]uint64, 32)
			distances := make([]float32, 32)
			for j := 0; j < 4; j++ {
				status, _ := api.search(segment.handle, query, 32, keys, distances)
				if status != 0 {
					failures <- fmt.Errorf("concurrent search: %s",
						api.errorMessage(segment.handle))
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}

	peak := api.handlePeakInFlight(segment.handle)
	if peak > int32(want) { //nolint:gosec // the bound is a small positive constant
		t.Fatalf("in-flight native calls peaked at %d, above the pool size %d", peak, want)
	}
	if runtime.GOMAXPROCS(0) > 1 && peak < 2 {
		t.Fatalf("searches never overlapped: peak in-flight %d", peak)
	}
	t.Logf("pool size %d, workers %d, peak in-flight calls %d", want, workers, peak)
}
