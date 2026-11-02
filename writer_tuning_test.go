package bluge

import (
	"context"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/RoaringBitmap/roaring/v2"

	"github.com/fy0/bluge/index"
	"github.com/fy0/bluge/index/mergeplan"
	"github.com/fy0/bluge/internal/zapxbluge"
	"github.com/fy0/bluge/segment"
)

func TestOnlineWriterMemoryTuning(t *testing.T) {
	const bufferSize = 64 * 1024
	options := mergeplan.DefaultMergePlanOptions
	options.SegmentsPerMergeTask = 2
	var plans, merges atomic.Int32
	options.CalcBudget = func(int64, int64, *mergeplan.Options) int {
		plans.Add(1)
		return 1
	}
	config := DefaultConfig(t.TempDir()).
		WithNumAnalysisWorkers(1).
		WithMergeBufferSize(bufferSize).
		WithMergePlanOptions(options)
	workers := 0
	config.indexConfig.GoFunc = func(f func()) {
		workers++
		go f()
	}
	config.indexConfig = config.indexConfig.WithSegmentPlugin(&index.SegmentPlugin{
		Type: zapxbluge.Type, Version: zapxbluge.Version,
		New: zapxbluge.New, Load: zapxbluge.Load,
		MergeWithOptions: func(segments []segment.Segment, drops []*roaring.Bitmap,
			size int, opts map[string]interface{}) segment.Merger {
			merges.Add(1)
			if size != bufferSize || len(segments) > 2 {
				t.Errorf("merge received buffer size %d and %d segments", size, len(segments))
			}
			return zapxbluge.MergeWithOptions(segments, drops, size, opts)
		},
	})
	writer, err := OpenWriter(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	if workers != 1 {
		t.Fatalf("started %d analysis workers, want 1", workers)
	}
	for i := 0; i < 16; i++ {
		batch := NewBatch()
		batch.Insert(NewDocument(strconv.Itoa(i)).AddField(NewTextField("body", "common term")))
		if err := writer.Batch(batch); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if plans.Load() == 0 || merges.Load() == 0 {
		t.Fatal("online writer did not plan and execute a merge")
	}
	reader, err := OpenReader(config)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	results, err := reader.Search(context.Background(), NewTopNSearch(16,
		NewTermQuery("common").SetField("body")).WithStandardAggregations())
	if err != nil {
		t.Fatal(err)
	}
	if count := results.Aggregations().Count(); count != 16 {
		t.Fatalf("reopened index matched %d documents, want 16", count)
	}
}

func TestOnlineWriterRejectsInvalidMemoryTuning(t *testing.T) {
	for _, test := range []struct {
		name   string
		config Config
		err    string
	}{
		{"zero workers", InMemoryOnlyConfig().WithNumAnalysisWorkers(0), "analysis workers"},
		{"negative workers", InMemoryOnlyConfig().WithNumAnalysisWorkers(-1), "analysis workers"},
		{"zero buffer", InMemoryOnlyConfig().WithMergeBufferSize(0), "merge buffer size"},
		{"negative buffer", InMemoryOnlyConfig().WithMergeBufferSize(-1), "merge buffer size"},
	} {
		t.Run(test.name, func(t *testing.T) {
			writer, err := OpenWriter(test.config)
			if writer != nil {
				_ = writer.Close()
				t.Fatal("invalid configuration opened a writer")
			}
			if err == nil || !strings.Contains(err.Error(), test.err) {
				t.Fatalf("got %v, want error mentioning %q", err, test.err)
			}
		})
	}
}
