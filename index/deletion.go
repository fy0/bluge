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

type DeletionPolicy interface {
	Commit(snapshot *Snapshot)
	Cleanup(Directory) error
}

// SegmentFileTracker is an optional interface a DeletionPolicy can implement
// to learn about the segment files present in the directory when a writer
// opens. It is only called after every snapshot in the directory was loaded
// successfully, so the set of referenced segments is complete and files not
// referenced by a live snapshot may be removed by Cleanup.
type SegmentFileTracker interface {
	TrackSegmentFiles(ids []uint64)
}

type KeepNLatestDeletionPolicy struct {
	n                 int
	liveEpochs        []uint64
	deletableEpochs   []uint64
	liveSegments      map[uint64]map[uint64]struct{}
	knownSegmentFiles map[uint64]struct{}
}

func NewKeepNLatestDeletionPolicy(n int) *KeepNLatestDeletionPolicy {
	return &KeepNLatestDeletionPolicy{
		n:                 n,
		liveSegments:      make(map[uint64]map[uint64]struct{}),
		knownSegmentFiles: make(map[uint64]struct{}),
	}
}

func (p *KeepNLatestDeletionPolicy) Commit(snapshot *Snapshot) {
	// build map of segments in this snapshot
	snapshotSegments := make(map[uint64]struct{})
	for _, segment := range snapshot.segment {
		snapshotSegments[segment.id] = struct{}{}
		p.knownSegmentFiles[segment.id] = struct{}{}
	}

	// add new epoch to the end
	p.liveEpochs = append(p.liveEpochs, snapshot.epoch)
	p.liveSegments[snapshot.epoch] = snapshotSegments

	// trim off epochs no longer needed and track separately
	if len(p.liveEpochs) > p.n {
		newlyDeletable := p.liveEpochs[:len(p.liveEpochs)-p.n]
		p.liveEpochs = p.liveEpochs[len(p.liveEpochs)-p.n:]
		p.deletableEpochs = append(p.deletableEpochs, newlyDeletable...)
	}
}

// TrackSegmentFiles marks the provided segment file ids as known, so that
// cleanupSegments can remove the ones not referenced by any live snapshot.
func (p *KeepNLatestDeletionPolicy) TrackSegmentFiles(ids []uint64) {
	for _, id := range ids {
		p.knownSegmentFiles[id] = struct{}{}
	}
}

func (p *KeepNLatestDeletionPolicy) Cleanup(dir Directory) error {
	p.cleanupSnapshots(dir)
	p.cleanupSegments(dir)
	return nil
}

func (p *KeepNLatestDeletionPolicy) cleanupSnapshots(dir Directory) {
	var remainingEpochs []uint64
	for _, deletableEpoch := range p.deletableEpochs {
		err := dir.Remove(ItemKindSnapshot, deletableEpoch)
		if err != nil {
			remainingEpochs = append(remainingEpochs, deletableEpoch)
		} else {
			delete(p.liveSegments, deletableEpoch)
		}
	}
	p.deletableEpochs = remainingEpochs
}

func (p *KeepNLatestDeletionPolicy) cleanupSegments(dir Directory) {
OUTER:
	for segmentID := range p.knownSegmentFiles {
		// check all of the live snapshots and see if this file is needed
		for _, segmentInSnapshot := range p.liveSegments {
			if _, ok := segmentInSnapshot[segmentID]; ok {
				// segment is still in use
				continue OUTER
			}
		}

		// file is no longer needed by anyone
		err := dir.Remove(ItemKindSegment, segmentID)
		if err != nil {
			// unable to remove, we'll try again next time
			continue
		}
		delete(p.knownSegmentFiles, segmentID)
	}
}
