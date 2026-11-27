//  Copyright (c) 2019 Couchbase, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//              http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package zap

import (
	"errors"
	"fmt"
)

const DefaultChunkMode uint32 = 1026
const docValueChunkSize uint64 = 1024

var ErrChunkSizeZero = errors.New("chunk size is zero")

func getChunkSize(chunkMode uint32, cardinality, maxDocs uint64) (uint64, error) {
	if chunkMode != DefaultChunkMode {
		return 0, fmt.Errorf("invalid chunk mode %d", chunkMode)
	}
	chunkSize := maxDocs / (cardinality/1024 + 1)
	if chunkSize == 0 {
		return 0, ErrChunkSizeZero
	}
	return chunkSize, nil
}
