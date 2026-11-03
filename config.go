//  Copyright (c) 2020 The Bluge Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// 		http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package bluge

import (
	"io/ioutil"
	"log"
	"time"

	"github.com/fy0/bluge/index"
	"github.com/fy0/bluge/index/mergeplan"

	"github.com/fy0/bluge/search"
	"github.com/fy0/bluge/search/similarity"

	"github.com/fy0/bluge/analysis"
	"github.com/fy0/bluge/analysis/analyzer"
)

type Config struct {
	indexConfig index.Config
	Logger      *log.Logger

	DefaultSearchField    string
	DefaultSearchAnalyzer *analysis.Analyzer
	DefaultSimilarity     search.Similarity
	PerFieldSimilarity    map[string]search.Similarity
	VectorBackend         VectorBackend

	SearchStartFunc func(size uint64) error
	SearchEndFunc   func(size uint64)

	maxConcurrentBatches  int
	maxDocumentInputBytes uint64
	maxBatchInputBytes    uint64
	batchStatsEnabled     bool
}

// WithVirtualField allows you to describe a field that
// the index will behave as if all documents in this index were
// indexed with these field/terms, even though nothing is
// physically persisted about them in the index.
func (config Config) WithVirtualField(field Field) Config {
	_ = field.Analyze(0)
	config.indexConfig = config.indexConfig.WithVirtualField(field)
	return config
}

func (config Config) WithSegmentType(typ string) Config {
	config.indexConfig = config.indexConfig.WithSegmentType(typ)
	return config
}

func (config Config) WithSegmentVersion(ver uint32) Config {
	config.indexConfig = config.indexConfig.WithSegmentVersion(ver)
	return config
}

// WithNumAnalysisWorkers sets the number of document analysis workers used by
// OpenWriter (default 4). OpenWriter rejects values below 1. Fewer workers can
// reduce concurrent analysis memory, but do not bound the size of a batch.
func (config Config) WithNumAnalysisWorkers(n int) Config {
	config.indexConfig.NumAnalysisWorkers = n
	return config
}

// WithMaxConcurrentBatches limits concurrent online Batch and BatchFunc calls
// through completion, including construction inside BatchFunc, analysis, and
// segment building. Zero (default) is unlimited.
// OpenWriter rejects negative values. Waiting calls are released by Close.
func (config Config) WithMaxConcurrentBatches(n int) Config {
	config.maxConcurrentBatches = n
	return config
}

// WithMaxDocumentInputBytes limits each document's pre-analysis sum of
// len(field.Name()) + len(field.Value()), including repeated fields and _id.
// Zero (default) disables the limit. This is not a memory limit.
func (config Config) WithMaxDocumentInputBytes(n uint64) Config {
	config.maxDocumentInputBytes = n
	return config
}

// WithMaxBatchInputBytes limits document input bytes plus the field and term
// bytes of every update/delete selector. Zero (default) disables the limit.
// Online Writer rejects the whole batch before analysis or index modification.
func (config Config) WithMaxBatchInputBytes(n uint64) Config {
	config.maxBatchInputBytes = n
	return config
}

// WithBatchStats enables online Writer.BatchStats workload gauges (default off).
// Measuring input bytes requires visiting fields before analysis.
func (config Config) WithBatchStats(enabled bool) Config {
	config.batchStatsEnabled = enabled
	return config
}

// WithMergeBufferSize sets the merge output buffer size in bytes (default 1 MiB).
// OpenWriter rejects values below 1. This is not a limit on total merge memory.
func (config Config) WithMergeBufferSize(bytes int) Config {
	config.indexConfig.MergeBufferSize = bytes
	return config
}

// WithMergePlanOptions replaces the online Writer's disk merge planner options.
// Start with mergeplan.DefaultMergePlanOptions and adjust the desired fields.
// Sizes are document counts unless WithMergePlanningByBytes is enabled. In byte
// mode, MaxSegmentSize also caps inputs to in-memory merges during persistence.
func (config Config) WithMergePlanOptions(options mergeplan.Options) Config {
	config.indexConfig.MergePlanOptions = options
	return config
}

// WithMergePlanningByBytes uses encoded segment bytes for merge planning instead
// of document counts. MergePlanOptions.MaxSegmentSize and FloorSegmentSize then
// use bytes. This helps avoid repeatedly rewriting segments with few large
// documents. MaxSegmentSize also bounds inputs to persistence's in-memory merges;
// larger groups are persisted directly and left to the disk planner.
//
// Sizes estimate live encoded data, not decoded workspace or process memory.
// Smaller maximums leave more segments to search. Existing oversized segments
// and individual batches are not split. Disabled by default.
func (config Config) WithMergePlanningByBytes(enabled bool) Config {
	config.indexConfig.MergePlanningByBytes = enabled
	return config
}

// WithOfflineWriterConcurrency sets the maximum number of concurrent segment
// builds and merge tasks used by OfflineWriter. It does not affect OpenWriter.
func (config Config) WithOfflineWriterConcurrency(concurrency int) Config {
	config.indexConfig = config.indexConfig.WithOfflineWriterConcurrency(concurrency)
	return config
}

// WithBackgroundRetryBackoff sets the initial and maximum delays between
// failed background merge and persistence attempts. Zero uses defaults of one
// second and one minute, respectively. Retries continue until success or close.
// OpenWriter rejects negative delays or a maximum below the initial delay.
func (config Config) WithBackgroundRetryBackoff(initialDelay, maxDelay time.Duration) Config {
	config.indexConfig = config.indexConfig.WithBackgroundRetryBackoff(initialDelay, maxDelay)
	return config
}

func (config Config) DisableOptimizeConjunction() Config {
	config.indexConfig = config.indexConfig.DisableOptimizeConjunction()
	return config
}

func (config Config) DisableOptimizeConjunctionUnadorned() Config {
	config.indexConfig = config.indexConfig.DisableOptimizeConjunctionUnadorned()
	return config
}

func (config Config) DisableOptimizeDisjunctionUnadorned() Config {
	config.indexConfig = config.indexConfig.DisableOptimizeDisjunctionUnadorned()
	return config
}

func (config Config) WithSearchStartFunc(f func(size uint64) error) Config {
	config.SearchStartFunc = f
	return config
}

func DefaultConfig(path string) Config {
	indexConfig := index.DefaultConfig(path)
	return defaultConfig(indexConfig)
}

func InMemoryOnlyConfig() Config {
	indexConfig := index.InMemoryOnlyConfig()
	return defaultConfig(indexConfig)
}
func DefaultConfigWithDirectory(df func() index.Directory) Config {
	indexConfig := index.DefaultConfigWithDirectory(df)
	return defaultConfig(indexConfig)
}

func defaultConfig(indexConfig index.Config) Config {
	rv := Config{
		Logger:                log.New(ioutil.Discard, "bluge", log.LstdFlags),
		DefaultSearchField:    "_all",
		DefaultSearchAnalyzer: analyzer.NewStandardAnalyzer(),
		DefaultSimilarity:     similarity.NewBM25Similarity(),
		PerFieldSimilarity:    map[string]search.Similarity{},
		VectorBackend:         unsupportedVectorBackend{},
	}

	allDocsFields := NewKeywordField("", "")
	_ = allDocsFields.Analyze(0)
	indexConfig = indexConfig.WithVirtualField(allDocsFields)
	indexConfig = indexConfig.WithNormCalc(rv.computeNorm)
	rv.indexConfig = indexConfig

	return rv
}

func (config Config) computeNorm(field string, length int) float32 {
	if pfs, ok := config.PerFieldSimilarity[field]; ok {
		return pfs.ComputeNorm(length)
	}
	return config.DefaultSimilarity.ComputeNorm(length)
}

func (config Config) indexConfigForWriting() index.Config {
	return config.indexConfig.WithNormCalc(config.computeNorm)
}
