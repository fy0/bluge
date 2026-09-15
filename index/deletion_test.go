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

package index

import (
	"fmt"
	"reflect"
	"testing"
)

func TestDeletableEpochs(t *testing.T) {
	tests := []struct {
		name            string
		n               int
		knownEpochs     []uint64
		deletableEpochs []uint64
	}{
		{
			name:            "empty",
			n:               1,
			knownEpochs:     nil,
			deletableEpochs: nil,
		},
		{
			name:            "one",
			n:               1,
			knownEpochs:     []uint64{1},
			deletableEpochs: nil,
		},
		{
			name:            "many",
			n:               1,
			knownEpochs:     []uint64{1, 2, 3, 4},
			deletableEpochs: []uint64{1, 2, 3},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(fmt.Sprintf("%s-%d", test.name, test.n), func(t *testing.T) {
			policy := NewKeepNLatestDeletionPolicy(test.n)
			for _, epoch := range test.knownEpochs {
				policy.Commit(&Snapshot{epoch: epoch})
			}
			if !reflect.DeepEqual(policy.deletableEpochs, test.deletableEpochs) {
				t.Errorf("expected deletable: %#v, got %#v", test.deletableEpochs, policy.deletableEpochs)
			}
		})
	}
}

// removeRecordingDirectory records segment Remove calls; all other
// Directory methods are unused by KeepNLatestDeletionPolicy.Cleanup.
type removeRecordingDirectory struct {
	Directory
	removed []uint64
}

func (d *removeRecordingDirectory) Remove(kind string, id uint64) error {
	if kind == ItemKindSegment {
		d.removed = append(d.removed, id)
	}
	return nil
}

// TestKeepNLatestTrackSegmentFiles verifies that segment files reported at
// open are removed by Cleanup only when no retained snapshot references
// them.
func TestKeepNLatestTrackSegmentFiles(t *testing.T) {
	dir := &removeRecordingDirectory{}
	policy := NewKeepNLatestDeletionPolicy(1)
	policy.Commit(&Snapshot{epoch: 1, segment: []*segmentSnapshot{{id: 7}}})
	policy.TrackSegmentFiles([]uint64{7, 8, 9})
	// age epoch 1 out of the retained window so segment 7 is no longer
	// referenced by a live snapshot
	policy.Commit(&Snapshot{epoch: 2, segment: []*segmentSnapshot{{id: 10}}})
	if err := policy.Cleanup(dir); err != nil {
		t.Fatal(err)
	}
	removed := map[uint64]bool{}
	for _, id := range dir.removed {
		removed[id] = true
	}
	if len(removed) != 3 || !removed[7] || !removed[8] || !removed[9] {
		t.Fatalf("expected segments 7, 8 and 9 removed, got %v", dir.removed)
	}
}
