//  Copyright (c) 2017 Couchbase, Inc.
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
	"hash/crc32"
	"io"
)

type CountHashWriter struct {
	w   io.Writer
	crc uint32
	n   uint64
}

func NewCountHashWriter(w io.Writer) *CountHashWriter {
	return &CountHashWriter{w: w}
}

func (c *CountHashWriter) Write(b []byte) (int, error) {
	n, err := c.w.Write(b)
	c.crc = crc32.Update(c.crc, crc32.IEEETable, b[:n])
	c.n += uint64(n)
	return n, err
}

func (c *CountHashWriter) Count() uint64 {
	return c.n
}

func (c *CountHashWriter) Sum32() uint32 {
	return c.crc
}
