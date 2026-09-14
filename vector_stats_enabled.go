// Copyright (c) 2026 The Bluge Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

//go:build blugevectorstats

package bluge

import "sync/atomic"

// Enabled with -tags blugevectorstats. The counters exist so benchmarks and
// diagnostic tests can report how many stored _id reads a vector search
// performs, which is the quantity the deferred-candidate-ID optimization
// targets.

const vectorStatsEnabled = true

var vectorStoredIDReads atomic.Uint64

func recordVectorStoredIDRead() { vectorStoredIDReads.Add(1) }

func vectorStoredIDReadCount() uint64 { return vectorStoredIDReads.Load() }

func resetVectorStoredIDReadCount() { vectorStoredIDReads.Store(0) }
