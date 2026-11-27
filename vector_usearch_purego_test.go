//go:build (windows || linux || darwin) && (amd64 || arm64)

package bluge

import (
	"fmt"
	"sync"
	"testing"
	"unsafe"
)

func TestUSearchGetResultCount(t *testing.T) {
	for _, count := range []uintptr{0, 1, 2} {
		api := &puregoUSearchAPI{
			indexGet: func(_ unsafe.Pointer, _ uint64, _ unsafe.Pointer, _ uintptr, resultCount *uintptr) int32 {
				*resultCount = count
				return 0
			},
		}
		if status := api.get(nil, 1, make([]float32, 2)); (status == 0) != (count == 1) {
			t.Fatalf("get returned status %d for count %d", status, count)
		}
	}
}

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

	t.Logf("pool size %d, concurrent workers %d", want, workers)
}
