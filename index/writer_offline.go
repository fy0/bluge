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
	"io"
	"runtime"
	"sort"
	"sync"

	"github.com/RoaringBitmap/roaring"

	segment "github.com/blugelabs/bluge_segment_api"
)

type WriterOffline struct {
	m         sync.Mutex
	config    Config
	directory Directory
	segPlugin *SegmentPlugin
	segCount  uint64
	segIDs    []uint64

	mergeMax            int
	buildTokens         chan struct{}
	builds              sync.WaitGroup
	buildErr            error
	closed              bool
	directoryConcurrent bool
	directoryMu         sync.Mutex
}

func OpenOfflineWriter(config Config) (writer *WriterOffline, err error) {
	return OpenOfflineWriterWithMergeMax(config, 10)
}

// OpenOfflineWriterWithMergeMax opens an offline writer with the maximum
// number of input segments combined by one merge task.
func OpenOfflineWriterWithMergeMax(config Config, maxSegmentsToMerge int) (writer *WriterOffline, err error) {
	if maxSegmentsToMerge < 2 {
		return nil, fmt.Errorf("max segments to merge must be at least 2")
	}
	if config.OfflineWriterConcurrency < 1 {
		return nil, fmt.Errorf("offline writer concurrency must be at least 1")
	}
	directory := config.DirectoryFunc()
	writer = &WriterOffline{
		config:              config,
		directory:           directory,
		mergeMax:            maxSegmentsToMerge,
		buildTokens:         make(chan struct{}, offlineConcurrency(config.OfflineWriterConcurrency)),
		directoryConcurrent: isConcurrentDirectory(directory),
	}

	err = writer.directory.Setup(false)
	if err != nil {
		return nil, fmt.Errorf("error setting up directory: %w", err)
	}

	writer.segPlugin, err = loadSegmentPlugin(config.supportedSegmentPlugins, config.SegmentType, config.SegmentVersion)
	if err != nil {
		return nil, fmt.Errorf("error loading segment plugin: %v", err)
	}

	return writer, nil
}

func (s *WriterOffline) Batch(batch *Batch) (err error) {
	if len(batch.documents) == 0 {
		return nil
	}
	docs := append([]segment.Document(nil), batch.documents...)
	for i, doc := range docs {
		if doc == nil {
			return fmt.Errorf("offline batch contains nil document at index %d", i)
		}
	}

	s.m.Lock()
	if s.closed {
		s.m.Unlock()
		return fmt.Errorf("offline writer is closed")
	}
	if s.buildErr != nil {
		err := s.buildErr
		s.m.Unlock()
		return err
	}
	segID := s.segCount
	s.segCount++
	s.builds.Add(1)
	s.m.Unlock()

	s.buildTokens <- struct{}{}
	go s.buildBatchSegment(segID, docs)
	return nil
}

func (s *WriterOffline) buildBatchSegment(segID uint64, docs []segment.Document) {
	err := s.buildBatchSegmentErr(segID, docs)
	<-s.buildTokens

	s.m.Lock()
	if err != nil {
		if s.buildErr == nil {
			s.buildErr = err
		}
	} else {
		s.segIDs = append(s.segIDs, segID)
	}
	s.m.Unlock()
	s.builds.Done()
}

func (s *WriterOffline) buildBatchSegmentErr(segID uint64, docs []segment.Document) error {
	for _, doc := range docs {
		doc.Analyze()
	}

	newSegment, _, err := s.segPlugin.New(docs, s.config.NormCalc)
	if err != nil {
		return err
	}
	if err := s.persist(ItemKindSegment, segID, newSegment); err != nil {
		return fmt.Errorf("error persisting segment %d: %w", segID, err)
	}
	return nil
}

func (s *WriterOffline) doMerge() error {
	for len(s.segIDs) > 1 {
		var tasks []offlineMergeTask
		nextSegIDs := make([]uint64, 0, (len(s.segIDs)+s.mergeMax-1)/s.mergeMax)
		for pos := 0; pos < len(s.segIDs); {
			remaining := len(s.segIDs) - pos
			if remaining == 1 {
				nextSegIDs = append(nextSegIDs, s.segIDs[pos])
				break
			}

			mergeCount := s.mergeMax
			if mergeCount > remaining {
				mergeCount = remaining
			}
			mergeIDs := append([]uint64(nil), s.segIDs[pos:pos+mergeCount]...)
			newID := s.segCount
			s.segCount++
			tasks = append(tasks, offlineMergeTask{ids: mergeIDs, newID: newID})
			nextSegIDs = append(nextSegIDs, newID)
			pos += mergeCount
		}

		if err := s.runMergeTasks(tasks); err != nil {
			return err
		}
		s.segIDs = nextSegIDs
	}

	return nil
}

type offlineMergeTask struct {
	ids   []uint64
	newID uint64
}

func (s *WriterOffline) runMergeTasks(tasks []offlineMergeTask) error {
	if len(tasks) == 0 {
		return nil
	}
	concurrency := offlineConcurrency(s.config.OfflineWriterConcurrency)
	if !s.directoryConcurrent {
		concurrency = 1
	}
	if len(tasks) == 1 || concurrency == 1 {
		for _, task := range tasks {
			if err := s.mergeSegments(task.ids, task.newID); err != nil {
				return err
			}
		}
		return nil
	}

	tokens := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	var errOnce sync.Once
	var firstErr error
	for _, task := range tasks {
		task := task
		tokens <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-tokens }()
			if err := s.mergeSegments(task.ids, task.newID); err != nil {
				errOnce.Do(func() { firstErr = err })
			}
		}()
	}
	wg.Wait()
	return firstErr
}

func (s *WriterOffline) mergeSegments(mergeIDs []uint64, newID uint64) error {
	mergeSegs := make([]segment.Segment, 0, len(mergeIDs))
	var closers []io.Closer
	closeOpenedSegs := func() error {
		var firstErr error
		for _, closer := range closers {
			if err := closer.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		return firstErr
	}

	for _, mergeID := range mergeIDs {
		data, closer, err := s.directory.Load(ItemKindSegment, mergeID)
		if err != nil {
			_ = closeOpenedSegs()
			return fmt.Errorf("error loading segment %d: %w", mergeID, err)
		}
		if closer != nil {
			closers = append(closers, closer)
		}
		seg, err := s.segPlugin.Load(data)
		if err != nil {
			_ = closeOpenedSegs()
			return fmt.Errorf("error loading segment %d: %w", mergeID, err)
		}
		mergeSegs = append(mergeSegs, seg)
	}

	drops := make([]*roaring.Bitmap, len(mergeIDs))
	merger := s.segPlugin.Merge(mergeSegs, drops, s.config.MergeBufferSize)
	if err := s.persist(ItemKindSegment, newID, merger); err != nil {
		_ = closeOpenedSegs()
		return fmt.Errorf("error merging segments %v: %w", mergeIDs, err)
	}
	if err := closeOpenedSegs(); err != nil {
		return fmt.Errorf("error closing merged segments %v: %w", mergeIDs, err)
	}
	for _, mergeID := range mergeIDs {
		if err := s.directory.Remove(ItemKindSegment, mergeID); err != nil {
			return fmt.Errorf("error removing segment %d after merge: %w", mergeID, err)
		}
	}
	return nil
}

func (s *WriterOffline) Close() error {
	s.m.Lock()
	if s.closed {
		s.m.Unlock()
		return fmt.Errorf("offline writer is closed")
	}
	s.closed = true
	s.m.Unlock()

	s.builds.Wait()

	s.m.Lock()
	if s.buildErr != nil {
		err := s.buildErr
		s.m.Unlock()
		return err
	}
	if len(s.segIDs) == 0 {
		s.m.Unlock()
		return fmt.Errorf("offline writer has no segments")
	}
	sort.Slice(s.segIDs, func(i, j int) bool { return s.segIDs[i] < s.segIDs[j] })
	s.m.Unlock()

	// perform all the merging into one segment
	err := s.doMerge()
	if err != nil {
		return fmt.Errorf("error while merging: %w", err)
	}

	// open the merged segment
	data, closer, err := s.directory.Load(ItemKindSegment, s.segIDs[0])
	if err != nil {
		return fmt.Errorf("error loading segment from directory: %w", err)
	}
	finalSeg, err := s.segPlugin.Load(data)
	if err != nil {
		if closer != nil {
			_ = closer.Close()
		}
		return fmt.Errorf("error loading segment: %w", err)
	}
	closeFinal := func() error {
		if closer == nil {
			return nil
		}
		return closer.Close()
	}

	// fake snapshot referencing this segment
	snapshot := &Snapshot{
		segment: []*segmentSnapshot{
			{
				id: s.segIDs[0],
				segment: &segmentWrapper{
					Segment:    finalSeg,
					refCounter: nil,
					persisted:  true,
				},
				segmentType:    s.segPlugin.Type,
				segmentVersion: s.segPlugin.Version,
			},
		},
		epoch: s.segIDs[0],
	}

	// persist the snapshot
	err = s.persist(ItemKindSnapshot, s.segIDs[0], snapshot)
	if err != nil {
		_ = closeFinal()
		return fmt.Errorf("error recording snapshot: %w", err)
	}
	if err := closeFinal(); err != nil {
		return fmt.Errorf("error closing final segment: %w", err)
	}
	return nil
}

func (s *WriterOffline) persist(kind string, id uint64, writer WriterTo) error {
	if !s.directoryConcurrent {
		s.directoryMu.Lock()
		defer s.directoryMu.Unlock()
	}
	return s.directory.Persist(kind, id, writer, nil)
}

func isConcurrentDirectory(directory Directory) bool {
	_, ok := directory.(concurrentDirectory)
	return ok
}

func offlineConcurrency(configured int) int {
	maxProcs := runtime.GOMAXPROCS(0)
	if configured > maxProcs {
		return maxProcs
	}
	return configured
}
