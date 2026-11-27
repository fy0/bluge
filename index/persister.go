//  Copyright (c) 2020 Couchbase, Inc.
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
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/RoaringBitmap/roaring/v2"

	segment "github.com/fy0/bluge/segment"
)

// Keep direct-persist reuse bounded so disk-backed roots do not retain an
// unbounded number of batch segments on the Go heap.
const (
	maxPersistedInMemorySegments = 4
	maxPersistedInMemoryBytes    = 8 * 1024 * 1024
)

type persisterState struct {
	epoch     uint64
	watchers  epochWatchers
	callbacks []func(error)
	syncs     []chan error
	retry     backgroundRetry
}

func (p *persisterState) resolveSyncs(err error) {
	for _, done := range p.syncs {
		if err != nil {
			done <- err
		} else {
			close(done)
		}
	}
	p.syncs = nil
}

func (s *Writer) persisterLoop(merges chan *segmentMerge, persists chan *persistIntroduction,
	introducerNotifier, persisterNotifier watcherChan, lastPersistedEpoch uint64) {
	defer s.asyncTasks.Done()
	state := persisterState{epoch: lastPersistedEpoch, retry: newBackgroundRetry(s.config)}
	defer state.retry.stop()
	introducerEpochWatcher, err := introducerNotifier.NotifyUsAfter(0, s.closeCh)
	if err != nil {
		return
	}
	var lastMergedEpoch uint64
	for {
		atomic.AddUint64(&s.stats.TotPersistLoopBeg, 1)
		atomic.AddUint64(&s.stats.TotPersistLoopWait, 1)
		notifyCh, retryCh := state.retry.notifications(introducerEpochWatcher.notifyCh)
		select {
		case <-s.closeCh:
			return
		case <-retryCh:
			state.retry.stop()
		case done := <-s.persistSyncs:
			if state.retry.err != nil {
				done <- state.retry.err
				continue
			}
			// Acknowledge only after the persisted epoch reaches root.
			state.syncs = append(state.syncs, done)
		case ew := <-persisterNotifier:
			state.watchers.Add(ew)
			lastMergedEpoch = ew.epoch
			state.watchers.NotifySatisfiedWatchers(state.epoch)
		case <-notifyCh:
			atomic.AddUint64(&s.stats.TotPersistLoopWaitNotified, 1)
			lastMergedEpoch, state.watchers = s.pausePersisterForMergerCatchUp(persisterNotifier,
				state.epoch, lastMergedEpoch, state.watchers)
			more, persistErr := s.persistPendingSnapshot(merges, persists, &state)
			if errors.Is(persistErr, segment.ErrClosed) {
				return
			}
			if persistErr != nil || more {
				continue
			}
			introducerEpochWatcher, err = introducerNotifier.NotifyUsAfter(state.epoch, s.closeCh)
			if err != nil {
				return
			}
			if err = s.deletionPolicy.Cleanup(s.directory); err != nil {
				s.config.AsyncError(err)
			}
		}
		if s.currentEpoch() == state.epoch {
			state.resolveSyncs(nil)
		}
		atomic.AddUint64(&s.stats.TotPersistLoopEnd, 1)
	}
}

func (s *Writer) takePendingSnapshot(epoch uint64) (*Snapshot, []chan error, []func(error)) {
	s.rootLock.Lock()
	defer s.rootLock.Unlock()
	if s.root == nil || s.root.epoch <= epoch {
		return nil, nil, nil
	}
	snapshot := s.root
	snapshot.addRef()
	persisted, callbacks := s.rootPersisted, s.persistedCallbacks
	s.rootPersisted, s.persistedCallbacks = nil, nil
	atomic.StoreUint64(&s.stats.persistSnapshotSize, uint64(snapshot.Size()))
	atomic.StoreUint64(&s.stats.persistEpoch, snapshot.epoch)
	return snapshot, persisted, callbacks
}

func (s *Writer) persistPendingSnapshot(merges chan *segmentMerge, persists chan *persistIntroduction,
	state *persisterState) (bool, error) {
	snapshot, persisted, callbacks := s.takePendingSnapshot(state.epoch)
	if snapshot == nil {
		return false, nil
	}
	startTime := time.Now()
	err := s.persistSnapshot(merges, persists, snapshot)
	for _, ch := range persisted {
		if err != nil {
			ch <- err
		}
		close(ch)
	}
	if err != nil {
		_ = snapshot.Close()
		atomic.StoreUint64(&s.stats.persistEpoch, 0)
		if errors.Is(err, segment.ErrClosed) {
			return false, err
		}
		state.callbacks = append(state.callbacks, callbacks...)
		atomic.AddUint64(&s.stats.TotPersistLoopErr, 1)
		s.fireAsyncError(state.retry.fail("persist snapshot", err))
		state.resolveSyncs(state.retry.err)
		return false, err
	}
	state.retry.reset()
	if state.callbacks != nil {
		callbacks = append(state.callbacks, callbacks...)
		state.callbacks = nil
	}
	for _, callback := range callbacks {
		callback(nil)
	}
	state.epoch = snapshot.epoch
	atomic.StoreUint64(&s.stats.LastPersistedEpoch, state.epoch)
	for _, watcher := range state.watchers {
		close(watcher.notifyCh)
	}
	state.watchers = nil
	_ = snapshot.Close()
	s.fireEvent(EventKindPersisterProgress, time.Since(startTime))
	more := s.currentEpoch() != state.epoch
	if more {
		atomic.AddUint64(&s.stats.TotPersistLoopProgress, 1)
	}
	return more, nil
}

func (s *Writer) pausePersisterForMergerCatchUp(persisterNotifier watcherChan,
	lastPersistedEpoch, lastMergedEpoch uint64, persistWatchers epochWatchers) (uint64, epochWatchers) {
	// First, let the watchers proceed if they lag behind
	persistWatchers.NotifySatisfiedWatchers(lastPersistedEpoch)

	fileCount := func() int {
		n, _ := s.directory.Stats()
		return int(n)
	}
	numFilesOnDisk := fileCount()

	// On finding fewer files on disk, persister takes a short pause
	// for sufficient in-memory segments to pile up for the next
	// memory merge cum persist loop.
	if numFilesOnDisk < s.config.PersisterNapUnderNumFiles &&
		s.config.PersisterNapTimeMSec > 0 && s.numEventsBlocking() == 0 {
		select {
		case <-s.closeCh:
		case <-time.After(time.Millisecond * time.Duration(s.config.PersisterNapTimeMSec)):
			atomic.AddUint64(&s.stats.TotPersisterNapPauseCompleted, 1)

		case ew := <-persisterNotifier:
			// unblock the merger in meantime
			persistWatchers.Add(ew)
			lastMergedEpoch = ew.epoch
			persistWatchers.NotifySatisfiedWatchers(lastPersistedEpoch)
			atomic.AddUint64(&s.stats.TotPersisterMergerNapBreak, 1)
		}
		return lastMergedEpoch, persistWatchers
	}

	// Finding too many files on disk could be due to two reasons.
	// 1. Too many older snapshots awaiting the clean up.
	// 2. The merger could be lagging behind on merging the disk files.
	if numFilesOnDisk > s.config.PersisterNapUnderNumFiles {
		err := s.deletionPolicy.Cleanup(s.directory)
		if err != nil {
			s.config.AsyncError(err)
		}
		numFilesOnDisk = fileCount()
	}

	// Persister pause until the merger catches up to reduce the segment
	// file count under the threshold.
	// But if there is memory pressure, then skip this sleep maneuvers.
OUTER:
	for s.config.PersisterNapUnderNumFiles > 0 &&
		numFilesOnDisk >= s.config.PersisterNapUnderNumFiles &&
		lastMergedEpoch < lastPersistedEpoch {
		atomic.AddUint64(&s.stats.TotPersisterSlowMergerPause, 1)

		select {
		case <-s.closeCh:
			break OUTER
		case ew := <-persisterNotifier:
			persistWatchers.Add(ew)
			lastMergedEpoch = ew.epoch
		}

		atomic.AddUint64(&s.stats.TotPersisterSlowMergerResume, 1)

		// let the watchers proceed if they lag behind
		persistWatchers.NotifySatisfiedWatchers(lastPersistedEpoch)

		numFilesOnDisk = fileCount()
	}

	return lastMergedEpoch, persistWatchers
}

func (s *Writer) persistSnapshot(merges chan *segmentMerge, persists chan *persistIntroduction, snapshot *Snapshot) error {
	// Perform in-memory segment merging only when the memory pressure is
	// below the configured threshold, else the persister performs the
	// direct persistence of segments.
	if s.numEventsBlocking() < s.config.MemoryPressurePauseThreshold {
		persisted, err := s.persistSnapshotMaybeMerge(merges, persists, snapshot)
		if err != nil {
			return err
		}
		if persisted {
			return nil
		}
	}

	return s.persistSnapshotDirect(persists, snapshot)
}

// persistSnapshotMaybeMerge examines the snapshot and might merge and
// persist the in-memory zap segments if there are enough of them
func (s *Writer) persistSnapshotMaybeMerge(merges chan *segmentMerge, persists chan *persistIntroduction, snapshot *Snapshot) (
	bool, error) {
	// collect the in-memory zap segments (SegmentBase instances)
	var sbs []segment.Segment
	var sbsDrops []*roaring.Bitmap
	var sbsIndexes []int
	var mergeBytes int64

	for i, segmentSnapshot := range snapshot.segment {
		if !segmentSnapshot.segment.Persisted() {
			if s.config.MergePlanningByBytes {
				size := segmentSnapshot.segment.mergeBytes()
				limit := s.config.MergePlanOptions.MaxSegmentSize
				if size > limit-mergeBytes {
					// Drain directly instead of adding merge workspace to a large
					// backlog of memory segments. Disk planning will group them.
					return false, nil
				}
				mergeBytes += size
			}
			sbs = append(sbs, segmentSnapshot.segment.Segment)
			sbsDrops = append(sbsDrops, segmentSnapshot.deleted)
			sbsIndexes = append(sbsIndexes, i)
		}
	}

	if len(sbs) < s.config.MinSegmentsForInMemoryMerge {
		return false, nil
	}

	newSnapshot, newSegmentID, err := s.mergeSegmentBases(
		merges, snapshot, sbs, sbsDrops, sbsIndexes)
	if err != nil {
		return false, err
	}
	if newSnapshot == nil {
		return false, nil
	}

	defer func() {
		_ = newSnapshot.Close()
	}()

	mergedSegmentIDs := map[uint64]struct{}{}
	for _, idx := range sbsIndexes {
		mergedSegmentIDs[snapshot.segment[idx].id] = struct{}{}
	}

	// construct a snapshot that's logically equivalent to the input
	// snapshot, but with merged segments replaced by the new segment
	equiv := &Snapshot{
		parent:  snapshot.parent,
		segment: make([]*segmentSnapshot, 0, len(snapshot.segment)),
		epoch:   snapshot.epoch,
		creator: "persistSnapshotMaybeMerge",
	}

	// copy to the equiv the segments that weren't replaced
	for _, segment := range snapshot.segment {
		if _, wasMerged := mergedSegmentIDs[segment.id]; !wasMerged {
			equiv.segment = append(equiv.segment, segment)
		}
	}

	// append to the equiv the new segment
	for _, segment := range newSnapshot.segment {
		if segment.id == newSegmentID {
			equiv.segment = append(equiv.segment, &segmentSnapshot{
				id:      newSegmentID,
				segment: segment.segment,
				deleted: nil, // nil since merging handled deletions
			})
			break
		}
	}

	err = s.persistSnapshotDirect(persists, equiv)
	if err != nil {
		return false, err
	}

	return true, nil
}

func (s *Writer) persistSnapshotDirect(persists chan *persistIntroduction, snapshot *Snapshot) (err error) {
	// first ensure that each segment in this snapshot has been persisted
	newSegments := make(map[uint64]*segmentWrapper)
	var persistedInMemorySegments int
	var persistedInMemoryBytes int
	for _, segmentSnapshot := range snapshot.segment {
		if segmentSnapshot.segment.Persisted() && segmentSnapshot.segment.inMemory {
			persistedInMemorySegments++
			persistedInMemoryBytes += segmentSnapshot.segment.Size()
		}
		if !segmentSnapshot.segment.Persisted() {
			err = s.directory.Persist(ItemKindSegment, segmentSnapshot.id, segmentSnapshot.segment.Segment, s.closeCh)
			if err != nil {
				return fmt.Errorf("error persisting segment: %w", err)
			}
			newSegments[segmentSnapshot.id] = segmentSnapshot.segment
		}
	}

	if len(newSegments) > 0 {
		persistedSegments := make(map[uint64]*segmentWrapper, len(newSegments))
		for segmentID, newSegment := range newSegments {
			segmentSize := newSegment.Size()
			if persistedInMemorySegments < maxPersistedInMemorySegments &&
				persistedInMemoryBytes+segmentSize <= maxPersistedInMemoryBytes {
				persistedSegments[segmentID] = newSegment.persistedView()
				persistedInMemorySegments++
				persistedInMemoryBytes += segmentSize
				continue
			}

			persistedSegments[segmentID], err = s.loadSegment(segmentID, s.segPlugin)
			if err != nil {
				for _, persistedSegment := range persistedSegments {
					if persistedSegment != nil {
						_ = persistedSegment.Close()
					}
				}
				return fmt.Errorf("error opening new segment %d, %v", segmentID, err)
			}
		}

		err = s.prepareIntroducePersist(persists, persistedSegments)
		if err != nil {
			return err
		}
	}

	err = s.directory.Persist(ItemKindSnapshot, snapshot.epoch, snapshot, s.closeCh)
	if err != nil {
		return err
	}

	s.deletionPolicy.Commit(snapshot)

	return nil
}

func (s *Writer) prepareIntroducePersist(persists chan *persistIntroduction,
	persistedSegments map[uint64]*segmentWrapper) error {
	defer func() {
		for _, s := range persistedSegments {
			if s != nil {
				// cleanup segments that were prepared but not
				// swapped into the new root
				_ = s.Close()
			}
		}
	}()

	persist := &persistIntroduction{
		persisted: persistedSegments,
		applied:   make(notificationChan),
	}

	select {
	case <-s.closeCh:
		return segment.ErrClosed
	case persists <- persist:
	}

	select {
	case <-s.closeCh:
		return segment.ErrClosed
	case <-persist.applied:
	}

	return nil
}
