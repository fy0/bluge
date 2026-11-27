//  Copyright (c) 2025 Couchbase, Inc.
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

package zap

import (
	"encoding/binary"
	"fmt"
	"sync"

	"github.com/blevesearch/vellum"
)

type invertedIndexCache struct {
	m     sync.RWMutex
	cache map[uint16]*vellum.FST
}

func newInvertedIndexCache() *invertedIndexCache {
	return &invertedIndexCache{cache: make(map[uint16]*vellum.FST)}
}

func (c *invertedIndexCache) Clear() {
	c.m.Lock()
	c.cache = nil
	c.m.Unlock()
}

func (c *invertedIndexCache) loadOrCreate(fieldID uint16, mem []byte) (*vellum.FST, uint64, error) {
	c.m.RLock()
	fst := c.cache[fieldID]
	c.m.RUnlock()
	if fst != nil {
		return fst, 0, nil
	}
	c.m.Lock()
	defer c.m.Unlock()
	if fst := c.cache[fieldID]; fst != nil {
		return fst, 0, nil
	}
	length, n := binary.Uvarint(mem)
	if n <= 0 || length == 0 || length > uint64(len(mem[n:])) {
		return nil, 0, fmt.Errorf("invalid dictionary length")
	}
	fst, err := vellum.Load(mem[n : uint64(n)+length])
	if err != nil {
		return nil, 0, err
	}
	c.cache[fieldID] = fst
	return fst, uint64(n) + length, nil
}
