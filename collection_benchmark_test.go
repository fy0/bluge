//go:build (windows || linux || darwin) && (amd64 || arm64)

package bluge

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fy0/bluge/index/mergeplan"
)

var collectionBenchOut = flag.String("collection-bench-out", "", "new output directory; enables the storage experiment")
var collectionBenchWorker = flag.String("collection-bench-worker", "", "internal: query worker config")
var collectionBenchDocs = flag.Int("collection-bench-docs", 100000, "synthetic documents")
var collectionBenchDims = flag.Int("collection-bench-dims", 768, "float32 dimensions")
var collectionBenchBatch = flag.Int("collection-bench-batch", 10000, "documents per initial batch")
var collectionBenchRounds = flag.Int("collection-bench-rounds", 20, "update rounds, each replacing 1 percent")
var collectionBenchQueries = flag.Int("collection-bench-queries", 64, "independent queries per model, three timed passes")
var collectionBenchCases = flag.String("collection-bench-cases", "embedded,sidecar,collection", "comma-separated backends")

type storageBenchConfig struct {
	Root, Backend, Library                                       string
	Documents, Dimensions, Batch, Rounds, Queries, Models, Phase int
	Seed                                                         uint64
	Options                                                      USearchVectorOptions
}

type storageBenchBytes struct {
	EmbeddedContainer int64 `json:"unsplit_embedded_container_bytes,omitempty"`
	Text              int64 `json:"text_bytes"`
	CurrentVector     int64 `json:"current_vector_bytes"`
	HistoryVector     int64 `json:"history_vector_bytes"`
	CurrentMetadata   int64 `json:"current_mapping_bytes"`
	HistoryMetadata   int64 `json:"history_mapping_bytes"`
	Control           int64 `json:"control_bytes"`
	Temporary         int64 `json:"temporary_bytes"`
	Total             int64 `json:"total_bytes"`
}

type storageBenchStep struct {
	Phase         string            `json:"phase"`
	Round         int               `json:"round"`
	Documents     int               `json:"documents"`
	TextMS        float64           `json:"text_ms"`
	VectorMS      float64           `json:"vector_ms"`
	TotalMS       float64           `json:"total_ms"`
	PeakBytes     int64             `json:"sampled_peak_bytes"`
	PeakTemporary int64             `json:"sampled_peak_temporary_bytes"`
	Samples       int               `json:"disk_samples"`
	Bytes         storageBenchBytes `json:"bytes"`
}

type storageBenchQuery struct {
	Clock              string      `json:"clock,omitempty"`
	QueriesPrepared    bool        `json:"queries_prepared_before_timing,omitempty"`
	OpenMS             float64     `json:"open_ms"`
	ProcessMS          float64     `json:"process_ms"`
	FirstMS            []float64   `json:"first_query_ms_by_model"`
	LatencyMS          [][]float64 `json:"resident_query_ms_by_model"`
	P50                []float64   `json:"p50_ms_by_model"`
	P95                []float64   `json:"p95_ms_by_model"`
	Recall             []float64   `json:"recall_at_10_by_model"`
	RecallQueries      [][]float64 `json:"recall_at_10_per_query"`
	Segments           int         `json:"segments"`
	DeletedRows        uint64      `json:"deleted_segment_rows"`
	EmbeddedPayload    int64       `json:"embedded_native_payload_bytes"`
	EmbeddedMapping    int64       `json:"embedded_envelope_mapping_bytes"`
	VerifiedNativeRows []int       `json:"verified_native_rows_by_model,omitempty"`
}

type storageBenchReport struct {
	RetainedGenerations                                                 int `json:"retained_generations,omitempty"`
	NativeABI                                                           uint32
	Config                                                              storageBenchConfig `json:"config"`
	Started                                                             string             `json:"started_utc"`
	Go, OS, Arch, CPU, LibrarySHA256, SourceCommit, MergeState, Dataset string
	CPUs, GOMAXPROCS                                                    int
	Hardware                                                            USearchHardwareInfo
	Command                                                             []string
	Steps                                                               []storageBenchStep `json:"steps"`
	InitialQuery, FinalQuery                                            storageBenchQuery
	InitialBytes, FinalBytes                                            storageBenchBytes
	BuildMS, UpdatesMS                                                  float64
	PinProbe                                                            []storageBenchStep `json:"pin_probe,omitempty"`
	PinQueryMS                                                          float64            `json:"pinned_lazy_query_ms,omitempty"`
	PinReleaseMS                                                        float64            `json:"pin_release_maintenance_ms,omitempty"`
	ReleasedBytes                                                       storageBenchBytes  `json:"released_bytes"`
}

// TestCollectionStorageBenchmark is deliberately opt-in, not a microbenchmark.
// Every case gets one text index and either one or two vector fields/spaces.
func TestCollectionStorageBenchmark(t *testing.T) {
	if *collectionBenchWorker != "" {
		storageBenchQueryWorker(t, *collectionBenchWorker)
		return
	}
	if *collectionBenchOut == "" {
		t.Skip("set -collection-bench-out to run the reproducible storage experiment")
	}
	if *collectionBenchDocs < 100 || *collectionBenchDims < 2 || *collectionBenchBatch < 1 || *collectionBenchQueries < 1 || *collectionBenchRounds < 0 {
		t.Fatal("invalid benchmark dimensions")
	}
	library, err := filepath.Abs(os.Getenv("BLUGE_USEARCH_LIBRARY_PATH"))
	if err != nil || os.Getenv("BLUGE_USEARCH_LIBRARY_PATH") == "" {
		t.Fatal("BLUGE_USEARCH_LIBRARY_PATH must name the native library")
	}
	root, err := filepath.Abs(*collectionBenchOut)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal("output must be a new directory:", err)
	}
	for _, backend := range strings.Split(*collectionBenchCases, ",") {
		if backend != "embedded" && backend != "sidecar" && backend != "collection" {
			t.Fatal("unknown backend", backend)
		}
		for _, models := range []int{1, 2} {
			cfg := storageBenchConfig{Root: filepath.Join(root, fmt.Sprintf("%s-%d", backend, models)), Backend: backend, Library: library,
				Documents: *collectionBenchDocs, Dimensions: *collectionBenchDims, Batch: *collectionBenchBatch,
				Rounds: *collectionBenchRounds, Queries: *collectionBenchQueries, Models: models, Seed: 20260918,
				Options: defaultUSearchVectorOptions()}
			t.Run(fmt.Sprintf("%s/%d", backend, models), func(t *testing.T) { storageBenchRun(t, cfg) })
		}
	}
}

func storageBenchTextConfig(c storageBenchConfig) Config {
	cfg := DefaultConfig(filepath.Join(c.Root, "data", "text"))
	cfg.indexConfig.MergePlanOptions.CalcBudget = func(int64, int64, *mergeplan.Options) int { return math.MaxInt }
	switch c.Backend {
	case "embedded":
		cfg = cfg.WithVectorBackend(&EmbeddedUSearchVectorBackend{libraryPath: c.Library, options: c.Options})
	case "sidecar":
		cfg = cfg.WithVectorBackend(NewUSearchVectorBackendWithOptions(filepath.Join(c.Root, "data", "vectors"), c.Options).WithLibraryPath(c.Library))
	}
	return cfg
}

func storageBenchCollectionConfig(c storageBenchConfig) CollectionConfig {
	cfg := DefaultCollectionConfig(filepath.Join(c.Root, "data"))
	cfg.TextConfig, cfg.LibraryPath, cfg.USearchOptions = storageBenchTextConfig(c), c.Library, c.Options
	return cfg
}

func storageBenchField(model int) string { return fmt.Sprintf("model-%d", model) }
func storageBenchID(id int) Identifier   { return Identifier(fmt.Sprintf("chunk-%08d", id)) }

// A fixed integer PRNG makes input independent of Go's math/rand version and
// native graph randomness. Queries are separate samples, never indexed vectors.
func storageBenchVector(c storageBenchConfig, id, revision, model int, query bool) []float32 {
	// #nosec G115 -- Benchmark ordinals seed modular uint64 arithmetic; wrapping is intentional.
	state := c.Seed ^ (uint64(id+1) * 0x9e3779b97f4a7c15) ^ (uint64(revision+1) * 0xbf58476d1ce4e5b9) ^ (uint64(model+1) * 0x94d049bb133111eb)
	if query {
		state ^= 0xd6e8feb86659fd93
	}
	v := make([]float32, c.Dimensions)
	for i := range v {
		state += 0x9e3779b97f4a7c15
		z := state
		z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
		z = (z ^ (z >> 27)) * 0x94d049bb133111eb
		z ^= z >> 31
		v[i] = float32(z>>40)/float32(1<<24) - 0.5
		// 64 overlapping topical clusters plus dense noise, with no quantization.
		if i%64 == id%64 {
			v[i] += 2
		}
	}
	return v
}

func storageBenchInputs(c storageBenchConfig, ids []int, revisions []int) ([]*Document, [][]VectorRecord) {
	docs := make([]*Document, len(ids))
	records := make([][]VectorRecord, c.Models)
	for m := range records {
		records[m] = make([]VectorRecord, len(ids))
	}
	for j, id := range ids {
		doc := NewDocument(string(storageBenchID(id))).AddField(NewKeywordField("group", fmt.Sprintf("topic-%02d", id%64))).
			AddField(NewTextField("body", fmt.Sprintf("synthetic audit document %d revision %d topic %d internal control revenue evidence sample", id, revisions[id], id%64)))
		for m := range records {
			vector := storageBenchVector(c, id, revisions[id], m, false)
			records[m][j] = VectorRecord{ID: storageBenchID(id), Vector: vector}
			if c.Backend != "collection" {
				doc.AddField(NewVectorField(storageBenchField(m), vector))
			}
		}
		docs[j] = doc
	}
	return docs, records
}

func storageBenchRun(t *testing.T, cfg storageBenchConfig) {
	if err := os.MkdirAll(cfg.Root, 0o755); err != nil {
		t.Fatal(err)
	}
	info, err := NewUSearchVectorBackendWithLibrary("unused", cfg.Library).HardwareAcceleration()
	if err != nil {
		t.Fatal(err)
	}
	hash, err := collectionFileHash(cfg.Library)
	if err != nil {
		t.Fatal(err)
	}
	commit, _ := exec.Command("git", "rev-parse", "HEAD").Output()
	cpu := os.Getenv("PROCESSOR_IDENTIFIER")
	if cpu == "" {
		cpu = "see environment.txt"
	}
	report := storageBenchReport{Config: cfg, Started: time.Now().UTC().Format(time.RFC3339), Go: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH,
		CPU: cpu, CPUs: runtime.NumCPU(), GOMAXPROCS: runtime.GOMAXPROCS(0), Hardware: info, LibrarySHA256: hash,
		SourceCommit: strings.TrimSpace(string(commit)), Command: os.Args, MergeState: "background merges disabled; durable batches; no forced merge",
		Dataset: "synthetic clustered dense float32 vectors and synthetic text; seed 20260918; cosine; K=10; fixed-seed random 1 percent updates per round; query samples independent of corpus; fresh process, OS cache not evicted"}
	api, err := loadUSearchAPI(cfg.Library)
	if err != nil {
		t.Fatal(err)
	}
	report.NativeABI = api.(*puregoUSearchAPI).abiVersion
	if cfg.Backend == "collection" {
		report.RetainedGenerations = 1
	}
	var writer *Writer
	var collection *Collection
	open := func() {
		if cfg.Backend == "collection" {
			collection, err = OpenCollection(storageBenchCollectionConfig(cfg))
			if err != nil {
				t.Fatal(err)
			}
			for m := 0; m < cfg.Models; m++ {
				field := storageBenchField(m)
				err = collection.CreateVectorSpace(VectorSpaceSpec{ID: field, Field: field, Model: field, ModelRevision: "synthetic-1", Preprocessing: "none", Dims: cfg.Dimensions, Similarity: VectorCosine})
				if err != nil {
					t.Fatal(err)
				}
			}
		} else {
			writer, err = OpenWriter(storageBenchTextConfig(cfg))
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	closeWriter := func() {
		if collection != nil {
			err = collection.Close()
		} else {
			err = writer.Close()
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	open()
	t.Cleanup(func() {
		if collection != nil {
			_ = collection.Close()
		}
		if writer != nil {
			_ = writer.Close()
		}
	})
	revisions := make([]int, cfg.Documents)
	write := func(ids []int, phase string, round int) {
		docs, vectors := storageBenchInputs(cfg, ids, revisions)
		monitor := storageBenchMonitorStart(filepath.Join(cfg.Root, "data"))
		step := storageBenchStep{Phase: phase, Round: round, Documents: len(ids)}
		start := time.Now()
		if collection != nil {
			revs, err := collection.UpsertDocuments(docs)
			if err != nil {
				t.Fatal(err)
			}
			step.TextMS = float64(time.Since(start).Microseconds()) / 1000
			vs := time.Now()
			for m := range vectors {
				for j := range vectors[m] {
					vectors[m][j].Revision = revs[j].Revision
				}
				if err := collection.UpsertVectors(storageBenchField(m), vectors[m]); err != nil {
					t.Fatal(err)
				}
				if err := collection.LastMaintenanceError(); err != nil {
					t.Fatal(err)
				}
			}
			step.VectorMS = float64(time.Since(vs).Microseconds()) / 1000
		} else {
			batch := NewBatch()
			for _, d := range docs {
				id, _, _ := documentIdentifier(d)
				batch.Update(id, d)
			}
			if err := writer.Batch(batch); err != nil {
				t.Fatal(err)
			}
		}
		step.TotalMS = float64(time.Since(start).Microseconds()) / 1000
		step.PeakBytes, step.PeakTemporary, step.Samples = monitor.stop()
		step.Bytes = storageBenchDisk(t, cfg)
		step.PeakBytes = max(step.PeakBytes, step.Bytes.Total)
		report.Steps = append(report.Steps, step)
		storageBenchJSON(t, filepath.Join(cfg.Root, "report.json"), report)
		t.Logf("%s models=%d %s %d: %.1f ms, disk=%d peak=%d", cfg.Backend, cfg.Models, phase, round, step.TotalMS, step.Bytes.Total, step.PeakBytes)
	}
	for start := 0; start < cfg.Documents; start += cfg.Batch {
		end := min(start+cfg.Batch, cfg.Documents)
		ids := make([]int, end-start)
		for j := range ids {
			ids[j] = start + j
		}
		write(ids, "build", start/cfg.Batch)
		report.BuildMS += report.Steps[len(report.Steps)-1].TotalMS
	}
	closeWriter()
	report.InitialQuery = storageBenchQueryProcess(t, cfg)
	report.InitialBytes = storageBenchDisk(t, cfg)
	storageBenchSplitEmbedded(&report.InitialBytes, report.InitialQuery)
	open()
	for round := 1; round <= cfg.Rounds; round++ {
		ids := storageBenchUpdateIDs(cfg, round)
		for _, id := range ids {
			revisions[id] = round
		}
		write(ids, "update", round)
		report.UpdatesMS += report.Steps[len(report.Steps)-1].TotalMS
	}
	closeWriter()
	cfg.Phase = cfg.Rounds
	report.FinalQuery = storageBenchQueryProcess(t, cfg)
	report.FinalBytes = storageBenchDisk(t, cfg)
	storageBenchSplitEmbedded(&report.FinalBytes, report.FinalQuery)
	if cfg.Backend == "collection" {
		open()
		pinned, err := OpenCollectionReader(collection.config)
		if err != nil {
			t.Fatal(err)
		}
		defer pinned.Close()
		// No text mutation: the probe isolates retention and lazy loading costs.
		for i := 0; i < 3; i++ {
			monitor := storageBenchMonitorStart(filepath.Join(cfg.Root, "data"))
			start := time.Now()
			for m := 0; m < cfg.Models; m++ {
				err := collection.UpsertVectors(storageBenchField(m), []VectorRecord{{ID: storageBenchID(0), Revision: collection.revisions[storageBenchID(0)], Vector: storageBenchVector(cfg, 0, revisions[0], m, false)}})
				if err != nil {
					t.Fatal(err)
				}
			}
			step := storageBenchStep{Phase: "pin-probe", Round: i + 1, TotalMS: float64(time.Since(start).Microseconds()) / 1000}
			step.PeakBytes, step.PeakTemporary, step.Samples = monitor.stop()
			step.Bytes = storageBenchDisk(t, cfg)
			step.PeakBytes = max(step.PeakBytes, step.Bytes.Total)
			report.PinProbe = append(report.PinProbe, step)
		}
		start := time.Now()
		for m := 0; m < cfg.Models; m++ {
			if _, err := pinned.SearchVectorRequest(context.Background(), storageBenchField(m), NewVectorSearchRequest(storageBenchField(m), storageBenchVector(cfg, 0, 0, m, true)).SetK(10)); err != nil {
				t.Fatal(err)
			}
		}
		report.PinQueryMS = float64(time.Since(start).Microseconds()) / 1000
		if err := pinned.Close(); err != nil {
			t.Fatal(err)
		}
		start = time.Now()
		if _, err := collection.Maintain(context.Background()); err != nil {
			t.Fatal(err)
		}
		report.PinReleaseMS = float64(time.Since(start).Microseconds()) / 1000
		report.ReleasedBytes = storageBenchDisk(t, cfg)
		closeWriter()
	}
	storageBenchJSON(t, filepath.Join(cfg.Root, "report.json"), report)
}

func storageBenchUpdateIDs(c storageBenchConfig, round int) []int {
	ids := make([]int, 0, c.Documents/100)
	seen := make(map[int]bool, cap(ids))
	// #nosec G115 -- Round ordinals seed modular uint64 arithmetic; wrapping is intentional.
	state := c.Seed ^ (uint64(round) * 0x9e3779b97f4a7c15)
	for len(ids) < cap(ids) {
		state = state*6364136223846793005 + 1442695040888963407
		// #nosec G115 -- Documents is the validated positive benchmark corpus size.
		id := int((state >> 32) % uint64(c.Documents))
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	sort.Ints(ids)
	return ids
}

func storageBenchJSON(t *testing.T, path string, value interface{}) {
	t.Helper()
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func storageBenchQueryProcess(t *testing.T, c storageBenchConfig) storageBenchQuery {
	path := filepath.Join(c.Root, fmt.Sprintf("query-%d-config.json", c.Phase))
	storageBenchJSON(t, path, c)
	// #nosec G204 G702 -- Relaunch this test binary with a fixed test and a locally generated config.
	cmd := exec.Command(os.Args[0], "-test.run=^TestCollectionStorageBenchmark$", "-test.timeout=30m", "-collection-bench-worker="+path)
	start := time.Now()
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("query child: %v\n%s", err, output)
	}
	var result storageBenchQuery
	if err := readCollectionJSON(path+".result.json", &result); err != nil {
		t.Fatal(err)
	}
	result.ProcessMS = float64(time.Since(start).Microseconds()) / 1000
	return result
}

func storageBenchQueryWorker(t *testing.T, path string) {
	var c storageBenchConfig
	if err := readCollectionJSON(path, &c); err != nil {
		t.Fatal(err)
	}
	result := storageBenchQuery{Clock: storageBenchClock, QueriesPrepared: true}
	queries := make([][][]float32, c.Models)
	for model := range queries {
		queries[model] = make([][]float32, c.Queries)
		for q := range queries[model] {
			queries[model][q] = storageBenchVector(c, q*1543, 0, model, true)
		}
	}
	var reader *Reader
	var collection *CollectionReader
	var err error
	start := storageBenchNow()
	if c.Backend == "collection" {
		collection, err = OpenCollectionReader(storageBenchCollectionConfig(c))
		if err == nil {
			defer collection.Close()
			reader = collection.text
		}
	} else {
		reader, err = OpenReader(storageBenchTextConfig(c))
		if err == nil {
			defer reader.Close()
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	result.OpenMS = storageBenchElapsedMS(start)
	result.Segments = len(reader.reader.Segments())
	for _, seg := range reader.reader.Segments() {
		if seg.Deleted() != nil {
			result.DeletedRows += seg.Deleted().GetCardinality()
		}
	}
	search := func(m, q int) []VectorHit {
		request := NewVectorSearchRequest(storageBenchField(m), queries[m][q]).SetK(10)
		var hits []VectorHit
		if collection != nil {
			hits, err = collection.SearchVectorRequest(context.Background(), storageBenchField(m), request)
		} else {
			hits, err = reader.SearchVectorRequest(context.Background(), request)
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 10 {
			t.Fatalf("ANN returned %d hits", len(hits))
		}
		return hits
	}
	for m := 0; m < c.Models; m++ {
		start := storageBenchNow()
		search(m, 0)
		result.FirstMS = append(result.FirstMS, storageBenchElapsedMS(start))
		var latencies []float64
		for pass := 0; pass < 3; pass++ {
			for q := 0; q < c.Queries; q++ {
				start := storageBenchNow()
				search(m, q)
				latencies = append(latencies, storageBenchElapsedMS(start))
			}
		}
		result.LatencyMS = append(result.LatencyMS, latencies)
		sorted := append([]float64(nil), latencies...)
		sort.Float64s(sorted)
		result.P50 = append(result.P50, sorted[(len(sorted)-1)*50/100])
		result.P95 = append(result.P95, sorted[(len(sorted)-1)*95/100])
	}
	if e, ok := reader.vector.(*embeddedUSearchVectorIndex); ok {
		for _, segments := range e.fields {
			for _, seg := range segments {
				p := seg.payload
				result.EmbeddedPayload += int64(len(p.Data))
				// Exact default (untransformed) zapx vector envelope encoding size.
				uvarint := func(n uint64) int64 { var buf [10]byte; return int64(binary.PutUvarint(buf[:], n)) }
				var docs int64
				for _, id := range p.DocIDs {
					docs += uvarint(uint64(id))
				}
				// #nosec G115 -- docs sums positive varint widths for this in-memory slice.
				result.EmbeddedMapping += 20 + 1 + uvarint(uint64(p.Dimensions)) + uvarint(uint64(len(p.DocIDs))) +
					uvarint(uint64(len(p.Backend))) + int64(len(p.Backend)) + uvarint(uint64(len(p.Similarity))) + int64(len(p.Similarity)) +
					uvarint(uint64(docs)) + docs + uvarint(uint64(len(p.Data)))
			}
		}
	}
	// Exact Flat results are calculated after timing, so they cannot warm ANN.
	revisions := make([]int, c.Documents)
	for round := 1; round <= c.Phase; round++ {
		for _, id := range storageBenchUpdateIDs(c, round) {
			revisions[id] = round
		}
	}
	for m := 0; m < c.Models; m++ {
		flat := newFlatVectorIndex("")
		field := storageBenchField(m)
		changes := make([]VectorChange, c.Documents)
		for id := range changes {
			changes[id] = VectorChange{ID: storageBenchID(id), Field: field, Similarity: VectorCosine, Vector: storageBenchVector(c, id, revisions[id], m, false)}
		}
		if err := flat.ApplyVectorChanges(changes); err != nil {
			t.Fatal(err)
		}
		// Validate persisted global-index key/value identity after timing. Recall
		// alone can hide lost or misassigned vectors after native compaction.
		var native *usearchVectorIndex
		if collection != nil {
			view := collection.views[field].vector.(*collectionVectorView)
			native, _ = view.raw.(*usearchVectorIndex)
		} else {
			native, _ = reader.vector.(*usearchVectorIndex)
		}
		if c.Backend != "embedded" && native == nil {
			t.Fatal("global native vector validation was not performed")
		}
		if native != nil {
			handle := native.indices[field]
			if native.api.size(handle) != c.Documents {
				t.Fatalf("native count=%d, expected %d", native.api.size(handle), c.Documents)
			}
			actual := make([]float32, c.Dimensions)
			for _, change := range changes {
				key := native.manifest.IDs[string(change.ID)]
				if key == 0 || native.api.get(handle, key, actual) != 0 {
					t.Fatalf("native vector missing: %s", change.ID)
				}
				for d, value := range actual {
					if value != change.Vector[d] {
						t.Fatalf("native vector mismatch: %s dimension %d", change.ID, d)
					}
				}
			}
			result.VerifiedNativeRows = append(result.VerifiedNativeRows, len(changes))
		}
		var recalls []float64
		var total float64
		for q := 0; q < c.Queries; q++ {
			exact, err := flat.Search(field, storageBenchVector(c, q*1543, 0, m, true), 10, nil)
			if err != nil {
				t.Fatal(err)
			}
			ids := make(map[Identifier]bool, len(exact))
			for _, h := range exact {
				ids[h.ID] = true
			}
			var matched int
			for _, hit := range search(m, q) {
				if ids[hit.ID] {
					matched++
				}
			}
			recall := float64(matched) / float64(len(exact))
			recalls = append(recalls, recall)
			total += recall
		}
		result.Recall = append(result.Recall, total/float64(c.Queries))
		result.RecallQueries = append(result.RecallQueries, recalls)
	}
	storageBenchJSON(t, path+".result.json", result)
}

func storageBenchSplitEmbedded(bytes *storageBenchBytes, q storageBenchQuery) {
	bytes.Text += bytes.EmbeddedContainer - q.EmbeddedPayload - q.EmbeddedMapping
	bytes.EmbeddedContainer = 0
	bytes.CurrentVector += q.EmbeddedPayload
	bytes.CurrentMetadata += q.EmbeddedMapping
}

func storageBenchDisk(t *testing.T, c storageBenchConfig) storageBenchBytes {
	t.Helper()
	root := filepath.Join(c.Root, "data")
	var b storageBenchBytes
	latest := make(map[string]uint64)
	committed := make(map[string]bool)
	if c.Backend == "collection" {
		for m := 0; m < c.Models; m++ {
			dir := collectionSpacePath(root, storageBenchField(m))
			var space collectionSpace
			if err := readCollectionJSON(filepath.Join(dir, "spec.json"), &space); err != nil {
				t.Fatal(err)
			}
			for _, n := range space.Generations {
				latest[dir] = max(latest[dir], n)
				idx, meta, commit := collectionGenerationFiles(dir, "usearch", n)
				committed[idx], committed[meta], committed[commit] = true, true, true
			}
		}
	}
	err := filepath.WalkDir(root, func(path string, e os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			return nil
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		n := info.Size()
		b.Total += n
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		name := e.Name()
		switch {
		case strings.Contains(rel, ".pending-") || strings.HasPrefix(name, ".bluge-"):
			b.Temporary += n
		case strings.HasPrefix(rel, "text/"):
			if c.Backend == "embedded" {
				b.EmbeddedContainer += n
			} else {
				b.Text += n
			}
		case c.Backend == "sidecar" && strings.HasSuffix(name, ".usearch"):
			b.CurrentVector += n
		case c.Backend == "sidecar" && name == "manifest.json":
			b.CurrentMetadata += n
		case c.Backend == "collection" && (strings.HasPrefix(name, "index-") || strings.HasPrefix(name, "metadata-") || strings.HasPrefix(name, "commit-")):
			if !committed[path] {
				b.Temporary += n
				break
			}
			if strings.HasPrefix(name, "commit-") {
				b.Control += n
				break
			}
			gen, _, err := collectionFileGeneration(name)
			if err != nil {
				return err
			}
			current := gen == latest[filepath.Dir(path)]
			if strings.HasPrefix(name, "index-") {
				if current {
					b.CurrentVector += n
				} else {
					b.HistoryVector += n
				}
			} else {
				if current {
					b.CurrentMetadata += n
				} else {
					b.HistoryMetadata += n
				}
			}
		default:
			b.Control += n
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type storageBenchMonitor struct {
	mu              sync.Mutex
	peak, temporary int64
	samples         int
	done, stopped   chan struct{}
	root            string
}

func storageBenchMonitorStart(root string) *storageBenchMonitor {
	m := &storageBenchMonitor{root: root, done: make(chan struct{}), stopped: make(chan struct{})}
	m.sample()
	go func() {
		defer close(m.stopped)
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-m.done:
				return
			case <-ticker.C:
				m.sample()
			}
		}
	}()
	return m
}

func (m *storageBenchMonitor) sample() {
	var total, temporary int64
	catalogs := make(map[string]*collectionSpace)
	_ = storageBenchWalk(m.root, func(path string, e os.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return nil
		}
		info, err := e.Info()
		if err != nil {
			return nil
		}
		total += info.Size()
		if strings.Contains(path, ".pending-") || strings.HasPrefix(e.Name(), ".bluge-") {
			temporary += info.Size()
		} else if strings.HasPrefix(filepath.Base(filepath.Dir(path)), "space-") &&
			(strings.HasPrefix(e.Name(), "index-") || strings.HasPrefix(e.Name(), "metadata-") || strings.HasPrefix(e.Name(), "commit-")) {
			// Final filenames can exist before the catalog publication point.
			// Count those files as temporary too, not just .pending directories.
			generation, recognized, err := collectionFileGeneration(e.Name())
			if err == nil && recognized {
				dir := filepath.Dir(path)
				space, loaded := catalogs[dir]
				if !loaded {
					var catalog collectionSpace
					data, err := storageBenchReadCatalog(filepath.Join(dir, "spec.json"))
					if err == nil && json.Unmarshal(data, &catalog) == nil {
						space = &catalog
					}
					catalogs[dir] = space
				}
				if space != nil {
					committed := false
					for _, n := range space.Generations {
						if n == generation {
							committed = true
							break
						}
					}
					if !committed {
						temporary += info.Size()
					}
				}
			}
		}
		return nil
	})
	m.mu.Lock()
	defer m.mu.Unlock()
	m.peak, m.temporary = max(m.peak, total), max(m.temporary, temporary)
	m.samples++
}

// Directory handles must share delete on Windows too: sampling a staging
// directory must not prevent the measured writer from retiring it.
func storageBenchWalk(root string, visit func(string, os.DirEntry, error) error) error {
	entries, err := storageBenchReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
		if entry.IsDir() {
			_ = storageBenchWalk(path, visit)
		} else if err := visit(path, entry, nil); err != nil {
			return err
		}
	}
	return nil
}

func (m *storageBenchMonitor) stop() (int64, int64, int) {
	close(m.done)
	<-m.stopped
	m.sample()
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.peak, m.temporary, m.samples
}

func TestStorageBenchmarkUpdateDistribution(t *testing.T) {
	c := storageBenchConfig{Documents: 100000, Dimensions: 768, Seed: 20260918}
	seen := make(map[int]bool)
	segments := make(map[int]bool)
	for _, id := range storageBenchUpdateIDs(c, 20) {
		if seen[id] || id < 0 || id >= c.Documents {
			t.Fatal("duplicate or out of range update id")
		}
		seen[id] = true
		segments[id/10000] = true
	}
	if len(seen) != 1000 {
		t.Fatal("update is not 1 percent")
	}
	if len(segments) != 10 {
		t.Fatal("updates do not span the initial segments")
	}
}
