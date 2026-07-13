// Copyright (c) 2020 The Bluge Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package segment

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestData(t *testing.T) {
	testCases := []struct {
		name       string
		input      []byte
		sliceStart int
		sliceEnd   int
	}{
		{name: "simple", input: []byte("simple")},
		{name: "kilo", input: bytes.Repeat([]byte{0}, 1024)},
		{name: "mega", input: bytes.Repeat([]byte{'m'}, 1024*1024)},
		{name: "simple-sliced", input: []byte("simple"), sliceEnd: 4},
		{name: "kilo-sliced", input: bytes.Repeat([]byte{0}, 1024), sliceStart: 4, sliceEnd: 1000},
		{name: "mega-sliced", input: bytes.Repeat([]byte{'m'}, 1024*1024), sliceStart: 27, sliceEnd: 1024*1024 - 48},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name+"-memory", func(t *testing.T) {
			assertData(t, NewDataBytes(testCase.input), testCase.input, testCase.sliceStart, testCase.sliceEnd)
		})

		t.Run(testCase.name+"-file", func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "data")
			if err := os.WriteFile(path, testCase.input, 0o600); err != nil {
				t.Fatal(err)
			}
			f, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			data, err := NewDataFile(f)
			if err != nil {
				t.Fatal(err)
			}
			assertData(t, data, testCase.input, testCase.sliceStart, testCase.sliceEnd)
		})
	}
}

func assertData(t *testing.T, data *Data, input []byte, start, end int) {
	t.Helper()
	expect := input
	if start != 0 || end != 0 {
		data = data.Slice(start, end)
		expect = input[start:end]
	}
	var buf bytes.Buffer
	n, err := data.WriteTo(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(expect)) || !bytes.Equal(buf.Bytes(), expect) {
		t.Fatalf("wrote %d bytes %v, expected %d bytes %v", n, buf.Bytes(), len(expect), expect)
	}
}
