// Copyright (c) 2026 The Bluge Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package bluge

import (
	"os"
	"reflect"
	"testing"
	"unsafe"
)

type fakeUSearchHandle struct {
	dimensions int
	vectors    map[uint64][]float32
}

type fakeUSearchAPI struct {
	addBatchKeys      [][]uint64
	removeBatchKeys   [][]uint64
	reserveCapacity   []int
	compactCalls      int
	saveCalls         int
	singleAddCalls    int
	singleRemoveCalls int
}

func (f *fakeUSearchAPI) create(dimensions, _, _, _, _ uintptr) unsafe.Pointer {
	handle := &fakeUSearchHandle{
		dimensions: int(dimensions),
		vectors:    make(map[uint64][]float32),
	}
	return unsafe.Pointer(handle)
}

func (f *fakeUSearchAPI) open(string) unsafe.Pointer       { return nil }
func (f *fakeUSearchAPI) openBuffer([]byte) unsafe.Pointer { return nil }
func (f *fakeUSearchAPI) destroy(unsafe.Pointer)           {}

func (f *fakeUSearchAPI) dimensions(handle unsafe.Pointer) int {
	return (*fakeUSearchHandle)(handle).dimensions
}

func (f *fakeUSearchAPI) size(handle unsafe.Pointer) int {
	return len((*fakeUSearchHandle)(handle).vectors)
}

func (f *fakeUSearchAPI) serializedLength(unsafe.Pointer) int { return 0 }
func (f *fakeUSearchAPI) saveBuffer(unsafe.Pointer, []byte) int32 {
	return 0
}

func (f *fakeUSearchAPI) reserve(_ unsafe.Pointer, capacity int) int32 {
	f.reserveCapacity = append(f.reserveCapacity, capacity)
	return 0
}

func (f *fakeUSearchAPI) add(unsafe.Pointer, uint64, []float32) int32 {
	f.singleAddCalls++
	return -1
}

func (f *fakeUSearchAPI) addBatch(handle unsafe.Pointer, keys []uint64,
	vectors []float32, dimensions int) int32 {
	f.addBatchKeys = append(f.addBatchKeys, append([]uint64(nil), keys...))
	native := (*fakeUSearchHandle)(handle)
	for i, key := range keys {
		start := i * dimensions
		native.vectors[key] = append([]float32(nil), vectors[start:start+dimensions]...)
	}
	return 0
}

func (f *fakeUSearchAPI) get(handle unsafe.Pointer, key uint64, vector []float32) int32 {
	values, ok := (*fakeUSearchHandle)(handle).vectors[key]
	if !ok || len(values) != len(vector) {
		return -1
	}
	copy(vector, values)
	return 0
}

func (f *fakeUSearchAPI) remove(unsafe.Pointer, uint64) int32 {
	f.singleRemoveCalls++
	return -1
}

func (f *fakeUSearchAPI) removeBatch(handle unsafe.Pointer, keys []uint64) int32 {
	f.removeBatchKeys = append(f.removeBatchKeys, append([]uint64(nil), keys...))
	native := (*fakeUSearchHandle)(handle)
	for _, key := range keys {
		delete(native.vectors, key)
	}
	return 0
}

func (f *fakeUSearchAPI) compact(unsafe.Pointer) int32 {
	f.compactCalls++
	return 0
}

func (f *fakeUSearchAPI) save(_ unsafe.Pointer, path string) int32 {
	f.saveCalls++
	if err := os.WriteFile(path, []byte("fake-usearch"), 0o600); err != nil {
		return -1
	}
	return 0
}

func (f *fakeUSearchAPI) search(unsafe.Pointer, []float32, int, []uint64,
	[]float32) (int32, int) {
	return 0, 0
}

func (f *fakeUSearchAPI) searchFiltered(unsafe.Pointer, []float32, int,
	[]uint64, []uint64, []float32) (int32, int) {
	return 0, 0
}

func (f *fakeUSearchAPI) errorMessage(unsafe.Pointer) string { return "fake native failure" }

func TestUSearchVectorIndexBatchesAndCoalescesMutations(t *testing.T) {
	api := &fakeUSearchAPI{}
	vectorIndex := &usearchVectorIndex{
		api:      api,
		root:     t.TempDir(),
		options:  defaultUSearchVectorOptions(),
		manifest: newUsearchManifest(),
		reverse:  make(map[uint64]Identifier),
		indices:  make(map[string]unsafe.Pointer),
	}
	defer vectorIndex.Close()

	err := vectorIndex.ApplyVectorChanges([]VectorChange{
		{ID: "one", Field: "embedding", Vector: []float32{1, 0}, Similarity: VectorCosine},
		{ID: "two", Field: "embedding", Vector: []float32{0, 1}, Similarity: VectorCosine},
		{ID: "one", Field: "embedding", Vector: []float32{0.25, 0.75}, Similarity: VectorCosine},
		{ID: "two", Delete: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if api.singleAddCalls != 0 || api.singleRemoveCalls != 0 {
		t.Fatalf("used scalar native mutations: add=%d remove=%d",
			api.singleAddCalls, api.singleRemoveCalls)
	}
	if !reflect.DeepEqual(api.removeBatchKeys, [][]uint64{{1, 2}}) {
		t.Fatalf("unexpected removal batches: %v", api.removeBatchKeys)
	}
	if !reflect.DeepEqual(api.addBatchKeys, [][]uint64{{1}}) {
		t.Fatalf("unexpected addition batches: %v", api.addBatchKeys)
	}
	if !reflect.DeepEqual(api.reserveCapacity, []int{1}) {
		t.Fatalf("unexpected reserve calls: %v", api.reserveCapacity)
	}
	if api.compactCalls != 1 || api.saveCalls != 1 {
		t.Fatalf("compact/save calls were %d/%d, expected 1/1",
			api.compactCalls, api.saveCalls)
	}
	handle := (*fakeUSearchHandle)(vectorIndex.indices["embedding"])
	if !reflect.DeepEqual(handle.vectors, map[uint64][]float32{1: {0.25, 0.75}}) {
		t.Fatalf("unexpected final native vectors: %v", handle.vectors)
	}
}

func TestUSearchBatchAdderBoundsRowsPerCall(t *testing.T) {
	api := &fakeUSearchAPI{}
	handle := api.create(1, 0, 0, 0, 0)
	adder := newUSearchBatchAdder(api, handle, 1)
	for key := uint64(1); key <= usearchBatchMaxRows+1; key++ {
		if err := adder.Add(key, []float32{float32(key)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := adder.Flush(); err != nil {
		t.Fatal(err)
	}
	if len(api.addBatchKeys) != 2 {
		t.Fatalf("got %d addition batches, expected 2", len(api.addBatchKeys))
	}
	if got := []int{len(api.addBatchKeys[0]), len(api.addBatchKeys[1])}; !reflect.DeepEqual(got, []int{usearchBatchMaxRows, 1}) {
		t.Fatalf("unexpected batch row counts: %v", got)
	}
}

var _ usearchNativeAPI = (*fakeUSearchAPI)(nil)
