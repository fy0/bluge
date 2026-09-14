// Copyright (c) 2026 The Bluge Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

//go:build !blugevectorstats

package bluge

// Vector hot-path counters are compiled out unless the blugevectorstats build
// tag is set. The no-op functions inline away, so normal builds pay nothing.

const vectorStatsEnabled = false

func recordVectorStoredIDRead() {}

func vectorStoredIDReadCount() uint64 { return 0 }

func resetVectorStoredIDReadCount() {}
