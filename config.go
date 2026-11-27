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
	"io"
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

func (config Config) WithNumAnalysisWorkers(n int) Config {
	config.indexConfig.NumAnalysisWorkers = n
	return config
}

func (config Config) WithMaxConcurrentBatches(n int) Config {
	config.maxConcurrentBatches = n
	return config
}

// Limits count names and values before analysis; zero disables them.
func (config Config) WithMaxDocumentInputBytes(n uint64) Config {
	config.maxDocumentInputBytes = n
	return config
}

func (config Config) WithMaxBatchInputBytes(n uint64) Config {
	config.maxBatchInputBytes = n
	return config
}

func (config Config) WithBatchStats(enabled bool) Config {
	config.batchStatsEnabled = enabled
	return config
}

func (config Config) WithMergeBufferSize(bytes int) Config {
	config.indexConfig.MergeBufferSize = bytes
	return config
}

func (config Config) WithMergePlanOptions(options mergeplan.Options) Config {
	config.indexConfig.MergePlanOptions = options
	return config
}

// The planner estimates live encoded bytes, including in-memory merge inputs.
func (config Config) WithMergePlanningByBytes(enabled bool) Config {
	config.indexConfig.MergePlanningByBytes = enabled
	return config
}

func (config Config) WithOfflineWriterConcurrency(concurrency int) Config {
	config.indexConfig = config.indexConfig.WithOfflineWriterConcurrency(concurrency)
	return config
}

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

const defaultSearchField = "_all"

func defaultConfig(indexConfig index.Config) Config {
	rv := Config{
		Logger:                log.New(io.Discard, "bluge", log.LstdFlags),
		DefaultSearchField:    defaultSearchField,
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
