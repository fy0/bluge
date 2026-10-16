//go:build (windows || linux || darwin) && (amd64 || arm64)

package bluge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"
	"unsafe"
)

type layoutExperimentTiming struct {
	P50MS     float64   `json:"p50_ms"`
	P95MS     float64   `json:"p95_ms"`
	Recall    float64   `json:"recall_at_10"`
	SamplesMS []float64 `json:"samples_ms"`
}

type layoutExperimentCase struct {
	Filter         string                 `json:"filter"`
	Comparisons    int                    `json:"ordered_key_and_distance_comparisons"`
	Mismatches     int                    `json:"mismatches"`
	Embedded       layoutExperimentTiming `json:"embedded_buffer"`
	Standalone     layoutExperimentTiming `json:"standalone_file"`
	PerQueryRecall []float64              `json:"per_query_recall_both_modes"`
	returnedKeys   [][]uint64
	allowedKeys    []uint64
	allowedIDs     map[Identifier]struct{}
}

// TestUSearchStorageLayoutExperiment isolates the native loading boundary:
// actual segment payload -> openBuffer versus identical bytes -> file -> open.
// It does not compare graph partitioning, Reader wrappers, GC, or cold I/O.
// Opt in with BLUGE_STORAGE_LAYOUT_EXPERIMENT_OUT pointing at a new JSON file.
func TestUSearchStorageLayoutExperiment(t *testing.T) {
	out := os.Getenv("BLUGE_STORAGE_LAYOUT_EXPERIMENT_OUT")
	if out == "" {
		t.Skip("set BLUGE_STORAGE_LAYOUT_EXPERIMENT_OUT to run the layout experiment")
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("report path must not exist: %s (%v)", out, err)
	}
	const documents, dimensions, queryCount, passes, k = 10000, 128, 100, 9, 10
	library := embeddedTestLibrary(t)
	vectorOf := func(i int) []float32 {
		return deterministicVector(uint64(i)+1, make([]float32, dimensions))
	}
	corpus := buildEmbeddedCorpus(t, embeddedCorpusSpec{
		Documents: documents, SegmentSize: documents, Dimensions: dimensions, VectorOf: vectorOf,
	})
	defer corpus.Close()
	segments := corpus.embeddedIndex(t).fields[embeddedBenchField]
	if len(segments) != 1 {
		t.Fatalf("expected one graph, got %d", len(segments))
	}
	seg := segments[0]
	path := filepath.Join(t.TempDir(), "same-graph.usearch")
	if err := os.WriteFile(path, seg.payload.Data, 0o600); err != nil {
		t.Fatal(err)
	}
	fileHash, err := collectionFileHash(path)
	if err != nil {
		t.Fatal(err)
	}
	payloadHash := sha256.Sum256(seg.payload.Data)
	if fileHash != hex.EncodeToString(payloadHash[:]) {
		t.Fatal("standalone file differs from the embedded native payload")
	}
	fileHandle := seg.api.open(path)
	if fileHandle == nil {
		t.Fatal("cannot open the identical graph from a standalone file")
	}
	defer seg.api.destroy(fileHandle)
	for _, handle := range []unsafe.Pointer{seg.handle, fileHandle} {
		if seg.api.size(handle) != documents || seg.api.dimensions(handle) != dimensions {
			t.Fatal("restored graph shape mismatch")
		}
	}
	ids := make(map[uint64]Identifier, documents)
	half := layoutExperimentCase{Filter: "50 percent of native keys", allowedIDs: make(map[Identifier]struct{})}
	for i, localDoc := range seg.payload.DocIDs {
		id, err := embeddedDocumentID(corpus.reader.reader, seg.offset+uint64(localDoc))
		if err != nil {
			t.Fatal(err)
		}
		key := uint64(i) + 1
		ids[key] = id
		if key%2 == 0 {
			half.allowedKeys = append(half.allowedKeys, key)
			half.allowedIDs[id] = struct{}{}
		}
	}
	queries := make([][]float32, queryCount)
	for i := range queries {
		queries[i] = vectorOf(documents + i)
	}
	cases := []layoutExperimentCase{{Filter: "none"}, half}
	for ci := range cases {
		c := &cases[ci]
		c.returnedKeys = make([][]uint64, queryCount)
		keysA, keysB := make([]uint64, k), make([]uint64, k)
		distA, distB := make([]float32, k), make([]float32, k)
		search := func(handle unsafe.Pointer, query []float32, keys []uint64, distances []float32) float64 {
			start := storageBenchNow()
			var status int32
			var count int
			if c.allowedKeys == nil {
				status, count = seg.api.search(handle, query, k, keys, distances)
			} else {
				status, count = seg.api.searchFiltered(handle, query, k, c.allowedKeys, keys, distances)
			}
			ms := storageBenchElapsedMS(start)
			if status != 0 || count != k {
				t.Fatalf("search failed: status=%d count=%d: %s", status, count, seg.api.errorMessage(handle))
			}
			return ms
		}
		// Warm both handles before measuring. Alternate order per query/pass.
		for _, query := range queries {
			search(seg.handle, query, keysA, distA)
			search(fileHandle, query, keysB, distB)
		}
		for pass := 0; pass < passes; pass++ {
			for qi, query := range queries {
				var a, b float64
				if (pass+qi)%2 == 0 {
					a = search(seg.handle, query, keysA, distA)
					b = search(fileHandle, query, keysB, distB)
				} else {
					b = search(fileHandle, query, keysB, distB)
					a = search(seg.handle, query, keysA, distA)
				}
				c.Embedded.SamplesMS = append(c.Embedded.SamplesMS, a)
				c.Standalone.SamplesMS = append(c.Standalone.SamplesMS, b)
				c.Comparisons++
				matched := slices.Equal(keysA, keysB)
				for j := range distA {
					matched = matched && math.Float32bits(distA[j]) == math.Float32bits(distB[j])
				}
				if !matched {
					c.Mismatches++
				}
				if pass == 0 {
					c.returnedKeys[qi] = slices.Clone(keysA)
				} else if !slices.Equal(c.returnedKeys[qi], keysA) {
					t.Fatal("query results changed across repeated searches")
				}
			}
		}
		for _, timing := range []*layoutExperimentTiming{&c.Embedded, &c.Standalone} {
			sorted := slices.Clone(timing.SamplesMS)
			slices.Sort(sorted)
			timing.P50MS = sorted[(len(sorted)-1)*50/100]
			timing.P95MS = sorted[(len(sorted)-1)*95/100]
		}
	}
	// Exact reference work happens after all timed searches.
	flat := newFlatVectorIndex("")
	changes := make([]VectorChange, documents)
	for i := range changes {
		changes[i] = VectorChange{ID: embeddedDocID(i), Field: embeddedBenchField,
			Similarity: VectorCosine, Vector: vectorOf(i)}
	}
	if err := flat.ApplyVectorChanges(changes); err != nil {
		t.Fatal(err)
	}
	for ci := range cases {
		c := &cases[ci]
		for qi, query := range queries {
			exact, err := flat.SearchCandidates(embeddedBenchField, query, k, c.allowedIDs)
			if err != nil || len(exact) != k {
				t.Fatalf("exact reference failed: %v", err)
			}
			want := make(map[Identifier]bool, k)
			for _, hit := range exact {
				want[hit.ID] = true
			}
			var hits int
			for _, key := range c.returnedKeys[qi] {
				if want[ids[key]] {
					hits++
				}
			}
			recall := float64(hits) / k
			c.PerQueryRecall = append(c.PerQueryRecall, recall)
			c.Embedded.Recall += recall / queryCount
		}
		// Identical ordered IDs imply identical recall. Do not infer this if the
		// comparison fails; preserve the report and fail below instead.
		if c.Mismatches == 0 {
			c.Standalone.Recall = c.Embedded.Recall
		}
		t.Logf("%s: comparisons=%d mismatches=%d recall=%.4f buffer p50/p95=%.4f/%.4f ms file=%.4f/%.4f ms",
			c.Filter, c.Comparisons, c.Mismatches, c.Embedded.Recall,
			c.Embedded.P50MS, c.Embedded.P95MS, c.Standalone.P50MS, c.Standalone.P95MS)
	}
	disk := measureLayoutExperimentDisk(t, corpus, ids, queries, library)
	libraryHash, err := collectionFileHash(library)
	if err != nil {
		t.Fatal(err)
	}
	sourceHash, err := collectionFileHash("vector_storage_layout_experiment_test.go")
	if err != nil {
		t.Fatal(err)
	}
	report := map[string]interface{}{
		"created_utc": time.Now().UTC(), "go": runtime.Version(), "os": runtime.GOOS,
		"arch": runtime.GOARCH, "gomaxprocs": runtime.GOMAXPROCS(0), "clock": storageBenchClock,
		"documents": documents, "dimensions": dimensions, "queries": queryCount, "passes": passes,
		"metric": "cosine", "hnsw": defaultUSearchVectorOptions(), "graph_count": 1,
		"native_bytes_each": len(seg.payload.Data), "native_sha256_both": fileHash,
		"library_sha256": libraryHash, "experiment_source_sha256": sourceHash, "cases": cases,
		"disk":  disk,
		"scope": "Same native graph extracted from a real embedded segment; embedded openBuffer versus standalone file open. Warm native search only; precomputed identical filters; alternating execution order. Disk comparison separately includes identical text documents, one native graph, and actual mapping/control files in each mode. No Reader-wrapper timing, generation-retention, build/update, memory-peak, or cold-I/O comparison.",
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := f.Write(append(data, '\n'))
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatalf("write report: %v; close: %v", writeErr, closeErr)
	}
	t.Logf("native payload: %d bytes in both modes; report: %s", len(seg.payload.Data), out)
	for _, c := range cases {
		if c.Mismatches != 0 {
			t.Errorf("%s: %d differing results", c.Filter, c.Mismatches)
		}
	}
}

// Build the same text corpus without vector fields, then attach the exact graph
// and its native-key/document-ID mapping using the existing sidecar format.
// Both indices contain one text segment, one model, and no update history.
func measureLayoutExperimentDisk(t *testing.T, corpus *embeddedCorpus, ids map[uint64]Identifier,
	queries [][]float32, library string) map[string]interface{} {
	t.Helper()
	seg := corpus.embeddedIndex(t).fields[embeddedBenchField][0]
	root := t.TempDir()
	textPath, vectorPath := filepath.Join(root, "text"), filepath.Join(root, "vectors")
	config := DefaultConfig(textPath)
	writer, err := OpenWriter(config)
	if err != nil {
		t.Fatal(err)
	}
	docs := make([]*Document, corpus.spec.Documents)
	for i := range docs {
		group := fmt.Sprintf("g%d", i%4)
		docs[i] = NewDocument(string(embeddedDocID(i))).
			AddField(NewKeywordField("group", group)).
			AddField(NewTextField("body", "chunk body "+group))
	}
	if err := writer.InsertMany(docs); err != nil {
		_ = writer.Close()
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(vectorPath, 0o700); err != nil {
		t.Fatal(err)
	}
	nativePath := usearchFieldPath(vectorPath, embeddedBenchField)
	if err := os.WriteFile(nativePath, seg.payload.Data, 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := newUsearchManifest()
	manifest.NextKey = uint64(len(ids)) + 1
	manifest.Fields[embeddedBenchField] = VectorFieldSpec{
		Name: embeddedBenchField, Dims: corpus.spec.Dimensions, Similarity: VectorCosine,
	}
	for key, id := range ids {
		manifest.IDs[string(id)] = key
	}
	if err := writeUsearchManifest(vectorPath, manifest); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenReader(config.WithVectorBackend(NewUSearchVectorBackendWithLibrary(vectorPath, library)))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	count, err := reader.Count()
	if err != nil || count != uint64(len(docs)) || len(reader.reader.Segments()) != 1 {
		t.Fatalf("sidecar text corpus mismatch: count=%d error=%v", count, err)
	}
	// Check the completed sidecar through its real public Reader, including ID
	// mapping, instead of reporting the size of an otherwise unusable file set.
	for _, query := range queries {
		request := NewVectorSearchRequest(embeddedBenchField, query).SetK(10)
		a, err := corpus.reader.SearchVectorRequest(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		b, err := reader.SearchVectorRequest(context.Background(), request)
		if err != nil || !slices.Equal(a, b) {
			t.Fatalf("complete sidecar Reader results differ: %v", err)
		}
	}
	embeddedFiles, embeddedTotal := corpus.config.indexConfig.DirectoryFunc().Stats()
	textFiles, textTotal := config.indexConfig.DirectoryFunc().Stats()
	metadata, err := os.Stat(usearchManifestPath(vectorPath))
	if err != nil {
		t.Fatal(err)
	}
	var standaloneTotal, standaloneFiles int64
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		standaloneTotal += info.Size()
		standaloneFiles++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	nativeBytes := int64(len(seg.payload.Data))
	if embeddedTotal <= uint64(nativeBytes) || textTotal == 0 ||
		standaloneTotal != int64(textTotal)+nativeBytes+metadata.Size() {
		t.Fatal("disk components do not reconcile")
	}
	delta := standaloneTotal - int64(embeddedTotal)
	t.Logf("complete disk: embedded=%d bytes (%d files), sidecar=%d bytes (%d files), delta=%d bytes (%.3f%%)",
		embeddedTotal, embeddedFiles, standaloneTotal, standaloneFiles, delta, float64(delta)*100/float64(embeddedTotal))
	return map[string]interface{}{
		"embedded_total_bytes": embeddedTotal, "embedded_files": embeddedFiles,
		"standalone_total_bytes": standaloneTotal, "standalone_files": standaloneFiles,
		"standalone_text_bytes": textTotal, "standalone_text_files": textFiles,
		"standalone_mapping_and_control_bytes": metadata.Size(), "native_bytes_each": nativeBytes,
		"embedded_extra_over_text_and_native_bytes": int64(embeddedTotal) - int64(textTotal) - nativeBytes,
		"standalone_minus_embedded_bytes":           delta,
		"standalone_extra_percent":                  float64(delta) * 100 / float64(embeddedTotal),
		"public_reader_query_pairs_verified":        len(queries),
		"scope":                                     "Logical file lengths, not allocated blocks. Identical synthetic text documents and identical native graph bytes. One text segment, one model, no update history or retained generations. Sidecar includes the existing JSON ID mapping and field metadata; embedded includes its in-segment mapping, vector envelope, and field metadata.",
	}
}
