// Copyright (c) 2026 The Bluge Authors.
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

package zap

import (
	"bytes"
	"testing"

	"github.com/RoaringBitmap/roaring/v2"
	scorchseg "github.com/blevesearch/scorch_segment_api/v2"
)

func postingsListFor(t *testing.T, sb *SegmentBase, field, term string) *PostingsList {
	t.Helper()
	dict, err := sb.dictionary(field)
	if err != nil {
		t.Fatal(err)
	}
	pl, err := dict.postingsList([]byte(term), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return pl
}

// onlyPosting returns the single posting of term in field, or nil.
func onlyPosting(t *testing.T, sb *SegmentBase, field, term string) *Posting {
	t.Helper()
	pl := postingsListFor(t, sb, field, term)
	itr := pl.Iterator(true, true, false, nil)
	p, err := itr.Next()
	if err != nil {
		t.Fatal(err)
	}
	if p == nil {
		return nil
	}
	posting, ok := p.(*Posting)
	if !ok {
		t.Fatalf("unexpected posting type %T", p)
	}
	if p, err := itr.Next(); err != nil || p != nil {
		t.Fatalf("expected a single posting for %q, second Next: %v %v",
			term, p, err)
	}
	return posting
}

// TestMergeOneHitNormGuard verifies that the merge path only emits the 1-hit
// posting encoding when the surviving posting's norm bits fit into 31 bits
// and are non-zero. FSTValEncode1Hit masks the norm to 31 bits, and readers
// treat a zero normBits as "not 1-hit" - so a card-1/freq-1 term carrying
// norm bits of 0 (or with the high bit set) used to be written as a 1-hit
// FST value that decoded to an empty postings list, silently losing the
// term.
func TestMergeOneHitNormGuard(t *testing.T) {
	constNorm := func(v float32) func(string, int) float32 {
		return func(string, int) float32 { return v }
	}
	for _, tc := range []struct {
		name     string
		normCalc func(string, int) float32 // nil = default norm
		want1Hit bool
	}{
		{"default norm stays 1-hit", nil, true},
		{"zero norm stays general", constNorm(0), false},
		{"norm high bit stays general", constNorm(float32(-1.5)), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// "alpha" is card-1/freq-1 with no locations in the merged
			// output; "shared" stays general in every variant.
			docs := newStatsTestBuildDocuments(t,
				newStatsTestDocument("a", "alpha shared"))
			var segA scorchseg.Segment
			var err error
			if tc.normCalc == nil {
				segA, _, err = New(docs)
			} else {
				segA, _, err = NewWithNormCalc(docs, tc.normCalc)
			}
			if err != nil {
				t.Fatal(err)
			}
			src := onlyPosting(t, segA.(*SegmentBase), "body", "alpha")
			if src == nil || src.Frequency() != 1 {
				t.Fatalf("source alpha posting: %v", src)
			}
			wantNorm := src.NormUint64()

			segB := newStatsTestSegment(t, newStatsTestDocument("b", "shared"))
			var buf bytes.Buffer
			_, _, err = MergeToWriter([]scorchseg.Segment{segA, segB},
				[]*roaring.Bitmap{nil, nil}, &buf, make(chan struct{}))
			if err != nil {
				t.Fatal(err)
			}
			merged, err := LoadBytes(buf.Bytes())
			if err != nil {
				t.Fatal(err)
			}
			sb := merged.(*SegmentBase)

			pl := postingsListFor(t, sb, "body", "alpha")
			if is1Hit := pl.normBits1Hit != 0; is1Hit != tc.want1Hit {
				t.Fatalf("alpha 1-hit: got %v want %v", is1Hit, tc.want1Hit)
			}
			p := onlyPosting(t, sb, "body", "alpha")
			if p == nil {
				t.Fatal("alpha has no posting")
			}
			if p.Frequency() != 1 {
				t.Fatalf("alpha freq: got %d want 1", p.Frequency())
			}
			if p.NormUint64() != wantNorm {
				t.Fatalf("alpha norm bits: got %x want %x",
					p.NormUint64(), wantNorm)
			}
		})
	}
}

// TestFSTVal1HitEncodingBoundaries covers the 31-bit packing limits used by
// the merge guard at the encoding layer (building >2^31 docs is
// impractical).
func TestFSTVal1HitEncodingBoundaries(t *testing.T) {
	for _, tc := range [][2]uint64{
		{0, 1},
		{1<<31 - 1, 1<<31 - 1},
		{12345, 0x3f9e0419},
	} {
		v := FSTValEncode1Hit(tc[0], tc[1])
		if v&FSTValEncodingMask != FSTValEncoding1Hit {
			t.Fatalf("encoded %x lost the 1-hit marker", v)
		}
		docNum, normBits := FSTValDecode1Hit(v)
		if docNum != tc[0] || normBits != tc[1] {
			t.Fatalf("round-trip (%d,%x): got (%d,%x)", tc[0], tc[1], docNum, normBits)
		}
	}
	if under32Bits(1 << 31) {
		t.Fatal("under32Bits(1<<31) must be false")
	}
	if !under32Bits(1<<31 - 1) {
		t.Fatal("under32Bits(1<<31 - 1) must be true")
	}
}
