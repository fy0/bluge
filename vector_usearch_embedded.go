package bluge

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"sync"
	"unsafe"

	"github.com/RoaringBitmap/roaring/v2"

	"github.com/fy0/bluge/index"
	zapxtext "github.com/fy0/bluge/internal/zapxtext"
	segment "github.com/fy0/bluge/segment"
)

type EmbeddedUSearchVectorBackend struct {
	libraryPath string
	options     USearchVectorOptions
}

func NewEmbeddedUSearchVectorBackend() *EmbeddedUSearchVectorBackend {
	return &EmbeddedUSearchVectorBackend{options: defaultUSearchVectorOptions()}
}

func (b *EmbeddedUSearchVectorBackend) Name() string { return vectorBackendUSearchName }

func (b *EmbeddedUSearchVectorBackend) WithLibraryPath(path string) *EmbeddedUSearchVectorBackend {
	copy := *b
	copy.libraryPath = path
	return &copy
}

func (b *EmbeddedUSearchVectorBackend) Open(Config) (VectorIndex, error) {
	return nil, errors.New("embedded usearch backend is opened from an index snapshot")
}

func (b *EmbeddedUSearchVectorBackend) BuildVectorPayload(field string,
	records []segment.VectorRecord) (segment.VectorPayload, error) {
	options := b.options
	api, err := loadUSearchAPI(b.libraryPath)
	if err != nil {
		return segment.VectorPayload{}, err
	}
	dimensions := len(records[0].Values)
	similarity := VectorSimilarity(records[0].Similarity)
	handle := api.create(uintptr(dimensions), usearchMetric(similarity),
		uintptr(options.Connectivity), uintptr(options.ExpansionAdd), uintptr(options.ExpansionSearch))
	if handle == nil {
		return segment.VectorPayload{}, fmt.Errorf("create embedded usearch field %q failed", field)
	}
	defer api.destroy(handle)
	if status := api.reserve(handle, len(records)); status != 0 {
		return segment.VectorPayload{}, usearchNativeStatus(api, handle, status, "reserve")
	}
	adder := newUSearchBatchAdder(api, handle, dimensions)
	for key := range uint64(len(records)) {
		if err := adder.Add(key+1, records[key].Values); err != nil {
			return segment.VectorPayload{}, err
		}
	}
	if err := adder.Flush(); err != nil {
		return segment.VectorPayload{}, err
	}
	data, err := serializeUSearchIndex(api, handle)
	if err != nil {
		return segment.VectorPayload{}, fmt.Errorf("serialize embedded usearch field %q: %w", field, err)
	}
	docIDs := make([]uint32, len(records))
	for i, record := range records {
		docIDs[i] = record.DocNum
	}
	return segment.VectorPayload{
		Backend:    b.Name(),
		Dimensions: uint32(dimensions),
		Similarity: string(similarity),
		DocIDs:     docIDs,
		Data:       data,
	}, nil
}

func (b *EmbeddedUSearchVectorBackend) MergeVectorPayload(field string,
	inputs []segment.VectorMergeInput) (segment.VectorPayload, error) {
	options := b.options
	api, err := loadUSearchAPI(b.libraryPath)
	if err != nil {
		return segment.VectorPayload{}, err
	}
	first := inputs[0].Payload
	dimensions := int(first.Dimensions)
	similarity := VectorSimilarity(first.Similarity)
	var live int
	for _, input := range inputs {
		for _, docID := range input.Payload.DocIDs {
			if input.NewDocNums[docID] != math.MaxUint64 {
				live++
			}
		}
	}
	if live == 0 {
		return segment.VectorPayload{}, nil
	}
	target := api.create(uintptr(dimensions), usearchMetric(similarity),
		uintptr(options.Connectivity), uintptr(options.ExpansionAdd), uintptr(options.ExpansionSearch))
	if target == nil {
		return segment.VectorPayload{}, fmt.Errorf("create merged embedded usearch field %q failed", field)
	}
	defer api.destroy(target)
	if status := api.reserve(target, live); status != 0 {
		return segment.VectorPayload{}, usearchNativeStatus(api, target, status, "reserve merge")
	}
	docIDs := make([]uint32, 0, live)
	key := uint64(1)
	adder := newUSearchBatchAdder(api, target, dimensions)
	for _, input := range inputs {
		mapped, err := mergeUSearchInput(api, adder, input, dimensions, &key)
		if err != nil {
			return segment.VectorPayload{}, err
		}
		docIDs = append(docIDs, mapped...)
	}
	if err := adder.Flush(); err != nil {
		return segment.VectorPayload{}, err
	}
	data, err := serializeUSearchIndex(api, target)
	if err != nil {
		return segment.VectorPayload{}, fmt.Errorf("serialize merged embedded field %q: %w", field, err)
	}
	return segment.VectorPayload{
		Backend:    b.Name(),
		Dimensions: first.Dimensions,
		Similarity: string(similarity),
		DocIDs:     docIDs,
		Data:       data,
	}, nil
}

func mergeUSearchInput(api usearchNativeAPI, adder *usearchBatchAdder,
	input segment.VectorMergeInput, dimensions int, key *uint64) ([]uint32, error) {
	source := api.openBuffer(input.Payload.Data)
	if source == nil {
		return nil, errors.New("open vector payload failed")
	}
	defer api.destroy(source)
	docIDs := make([]uint32, 0, len(input.Payload.DocIDs))
	vector := make([]float32, dimensions)
	for sourceKey := range uint64(len(input.Payload.DocIDs)) {
		newDocID := input.NewDocNums[input.Payload.DocIDs[sourceKey]]
		if newDocID == math.MaxUint64 {
			continue
		}
		if status := api.get(source, sourceKey+1, vector); status != 0 {
			return nil, usearchNativeStatus(api, source, status, "get merge")
		}
		if err := adder.Add(*key, vector); err != nil {
			return nil, err
		}
		*key++
		docIDs = append(docIDs, uint32(newDocID))
	}
	return docIDs, nil
}

func serializeUSearchIndex(api usearchNativeAPI, handle unsafe.Pointer) ([]byte, error) {
	length := api.serializedLength(handle)
	if length <= 0 {
		return nil, errors.New("native index has no serialized bytes")
	}
	data := make([]byte, length)
	if status := api.saveBuffer(handle, data); status != 0 {
		return nil, usearchNativeStatus(api, handle, status, "save buffer")
	}
	return data, nil
}

type embeddedUSearchVectorIndex struct {
	mu       sync.RWMutex
	snapshot *index.Snapshot
	fields   map[string][]embeddedUSearchSegment
	closed   bool
}

type embeddedUSearchSegment struct {
	api           usearchNativeAPI
	handle        unsafe.Pointer
	payload       segment.VectorPayload
	offset        uint64
	documentCount uint64
	deleted       *roaring.Bitmap
}

type vectorSegmentUnwrapper interface {
	UnderlyingSegment() segment.Segment
}

type vectorPayloadReader interface {
	VectorPayload(string) (segment.VectorPayload, error)
}

type snapshotSegmentAccess interface {
	Segment() segment.Segment
	Deleted() *roaring.Bitmap
}

func visitVectorPayloads(snapshot *index.Snapshot,
	visit func(segment.Segment, *roaring.Bitmap, uint64, string, segment.VectorPayload) error) error {
	var offset uint64
	for _, candidate := range snapshot.Segments() {
		access := candidate.(snapshotSegmentAccess)
		inner := access.Segment()
		if unwrap, ok := inner.(vectorSegmentUnwrapper); ok {
			inner = unwrap.UnderlyingSegment()
		}
		reader, ok := inner.(vectorPayloadReader)
		if ok {
			for _, field := range inner.Fields() {
				payload, err := reader.VectorPayload(field)
				if errors.Is(err, zapxtext.ErrVectorPayloadNotFound) {
					continue
				}
				if err != nil {
					return err
				}
				if err := visit(inner, access.Deleted(), offset, field, payload); err != nil {
					return err
				}
			}
		}
		offset += inner.Count()
	}
	return nil
}

func (b *EmbeddedUSearchVectorBackend) VectorFieldSpecs(snapshot *index.Snapshot) (map[string]VectorFieldSpec, error) {
	fields := make(map[string]VectorFieldSpec)
	err := visitVectorPayloads(snapshot,
		func(_ segment.Segment, _ *roaring.Bitmap, _ uint64, field string, payload segment.VectorPayload) error {
			spec := VectorFieldSpec{Name: field, Dims: int(payload.Dimensions), Similarity: VectorSimilarity(payload.Similarity)}
			if previous, exists := fields[field]; exists && previous != spec {
				return fmt.Errorf("inconsistent vector field %q", field)
			}
			fields[field] = spec
			return nil
		})
	return fields, err
}

func (b *EmbeddedUSearchVectorBackend) OpenSnapshot(snapshot *index.Snapshot) (VectorIndex, error) {
	api, err := loadUSearchAPI(b.libraryPath)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrVectorUnsupported, err)
	}
	rv := &embeddedUSearchVectorIndex{snapshot: snapshot, fields: make(map[string][]embeddedUSearchSegment)}
	err = visitVectorPayloads(snapshot,
		func(inner segment.Segment, deleted *roaring.Bitmap, offset uint64, field string, payload segment.VectorPayload) error {
			if payload.Backend != b.Name() {
				return fmt.Errorf("unsupported embedded vector backend %q", payload.Backend)
			}
			handle := api.openBuffer(payload.Data)
			if handle == nil {
				return fmt.Errorf("open embedded vector field %q failed", field)
			}
			rv.fields[field] = append(rv.fields[field], embeddedUSearchSegment{
				api: api, handle: handle, payload: payload, offset: offset,
				documentCount: inner.Count(), deleted: deleted,
			})
			return nil
		})
	if err != nil {
		_ = rv.Close()
		return nil, err
	}
	return rv, nil
}

func (e *embeddedUSearchVectorIndex) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}
	e.closed = true
	for field, segments := range e.fields {
		for _, segment := range segments {
			segment.api.destroy(segment.handle)
		}
		delete(e.fields, field)
	}
	return nil
}

func (e *embeddedUSearchVectorIndex) Search(field string, query []float32, k int,
	filter Query) ([]VectorHit, error) {
	if filter != nil {
		return nil, ErrVectorFilterUnsupported
	}
	return e.SearchCandidates(field, query, k, nil)
}

func (e *embeddedUSearchVectorIndex) SearchCandidates(field string, query []float32,
	k int, allowed map[Identifier]struct{}) ([]VectorHit, error) {
	if err := validateVectorQuery(query, k); err != nil {
		return nil, err
	}
	return e.searchVectorCandidates(field, query, k, allowed)
}

func (e *embeddedUSearchVectorIndex) searchVectorCandidates(field string, query []float32,
	k int, allowed map[Identifier]struct{}) ([]VectorHit, error) {
	return e.searchCandidates(field, query, k, allowed, nil)
}

func (e *embeddedUSearchVectorIndex) searchDocumentCandidates(field string, query []float32,
	k int, allowed []uint64) ([]VectorHit, error) {
	return e.searchCandidates(field, query, k, nil, allowed)
}

// Stored IDs are resolved only for filtering and candidates that can survive ranking.
type embeddedVectorCandidate struct {
	globalDoc uint64
	score     float64
	id        Identifier
	idSet     bool
}

func (e *embeddedUSearchVectorIndex) searchCandidates(field string, query []float32,
	k int, allowedIDs map[Identifier]struct{}, allowedDocs []uint64) ([]VectorHit, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.closed {
		return nil, errors.New("embedded usearch vector index is closed")
	}
	segments := e.fields[field]
	if len(segments) == 0 {
		return nil, fmt.Errorf("%w: %q", ErrVectorFieldNotFound, field)
	}
	if int(segments[0].payload.Dimensions) != len(query) {
		return nil, fmt.Errorf("%w: field %q", ErrVectorInvalidDimension, field)
	}
	if (allowedIDs != nil && len(allowedIDs) == 0) ||
		(allowedDocs != nil && len(allowedDocs) == 0) {
		return []VectorHit{}, nil
	}
	candidates, err := e.collectCandidates(segments, query, k, allowedIDs, allowedDocs)
	if err != nil {
		return nil, err
	}
	return e.rankCandidates(candidates, k)
}

func (e *embeddedUSearchVectorIndex) collectCandidates(segments []embeddedUSearchSegment,
	query []float32, k int, allowedIDs map[Identifier]struct{},
	allowedDocs []uint64) ([]embeddedVectorCandidate, error) {
	candidates := make([]embeddedVectorCandidate, 0, len(segments)*k)
	for _, segment := range segments {
		allowedKeys, filtered := segment.allowedKeys(allowedDocs)
		if allowedIDs != nil {
			var err error
			allowedKeys, err = e.allowedIDKeys(segment, allowedKeys, filtered, allowedIDs)
			if err != nil {
				return nil, err
			}
			filtered = true
		}
		if filtered && len(allowedKeys) == 0 {
			continue
		}
		keys, distances, err := searchUSearch(segment.api, segment.handle, query, k, allowedKeys)
		if err != nil {
			return nil, err
		}
		similarity := VectorSimilarity(segment.payload.Similarity)
		for i, key := range keys {
			localDoc := segment.payload.DocIDs[key-1]
			candidates = append(candidates, embeddedVectorCandidate{
				globalDoc: segment.offset + uint64(localDoc),
				score:     usearchScore(similarity, float64(distances[i])),
			})
		}
	}
	return candidates, nil
}

func (e *embeddedUSearchVectorIndex) allowedIDKeys(s embeddedUSearchSegment, keys []uint64,
	filtered bool, allowed map[Identifier]struct{}) ([]uint64, error) {
	if !filtered {
		keys = make([]uint64, len(s.payload.DocIDs))
		for i := range uint64(len(keys)) {
			keys[i] = i + 1
		}
	}
	selected := keys[:0]
	for _, key := range keys {
		id, err := embeddedDocumentID(e.snapshot, s.offset+uint64(s.payload.DocIDs[key-1]))
		if err != nil {
			return nil, err
		}
		if _, ok := allowed[id]; ok {
			selected = append(selected, key)
		}
	}
	return selected, nil
}

// Ties use stored IDs; candidates below the score cutoff need no stored-field reads.
func (e *embeddedUSearchVectorIndex) rankCandidates(candidates []embeddedVectorCandidate,
	k int) ([]VectorHit, error) {
	if len(candidates) <= k {
		if err := e.resolveCandidateIDs(candidates); err != nil {
			return nil, err
		}
		slices.SortFunc(candidates, compareScoreThenID)
		return vectorHits(candidates, nil), nil
	}
	slices.SortFunc(candidates, func(a, b embeddedVectorCandidate) int {
		return cmp.Compare(b.score, a.score)
	})
	cutoff := candidates[k-1].score
	above := sort.Search(len(candidates), func(i int) bool {
		return candidates[i].score <= cutoff
	})
	tiedEnd := above + sort.Search(len(candidates)-above, func(i int) bool {
		return candidates[above+i].score < cutoff
	})
	selected, tied := candidates[:above], candidates[above:tiedEnd]
	if err := e.resolveCandidateIDs(selected); err != nil {
		return nil, err
	}
	if err := e.resolveCandidateIDs(tied); err != nil {
		return nil, err
	}
	slices.SortFunc(selected, compareScoreThenID)
	slices.SortFunc(tied, func(a, b embeddedVectorCandidate) int {
		return cmp.Compare(a.id, b.id)
	})
	keep := k - len(selected)
	return vectorHits(selected, tied[:keep]), nil
}

func compareScoreThenID(a, b embeddedVectorCandidate) int {
	if byScore := cmp.Compare(b.score, a.score); byScore != 0 {
		return byScore
	}
	return cmp.Compare(a.id, b.id)
}

func vectorHits(head, tail []embeddedVectorCandidate) []VectorHit {
	hits := make([]VectorHit, 0, len(head)+len(tail))
	for _, candidate := range head {
		hits = append(hits, VectorHit{ID: candidate.id, Score: candidate.score})
	}
	for _, candidate := range tail {
		hits = append(hits, VectorHit{ID: candidate.id, Score: candidate.score})
	}
	return hits
}

func (e *embeddedUSearchVectorIndex) resolveCandidateIDs(
	candidates []embeddedVectorCandidate) error {
	for i := range candidates {
		if candidates[i].idSet {
			continue
		}
		id, err := embeddedDocumentID(e.snapshot, candidates[i].globalDoc)
		if err != nil {
			return err
		}
		candidates[i].id = id
		candidates[i].idSet = true
	}
	return nil
}

func (s embeddedUSearchSegment) allowedKeys(allowedDocs []uint64) ([]uint64, bool) {
	if allowedDocs == nil {
		if s.deleted == nil || s.deleted.IsEmpty() {
			return nil, false
		}
		keys := make([]uint64, 0, len(s.payload.DocIDs))
		for key := range uint64(len(s.payload.DocIDs)) {
			if !s.deleted.Contains(s.payload.DocIDs[key]) {
				keys = append(keys, key+1)
			}
		}
		return keys, true
	}

	first := sort.Search(len(allowedDocs), func(i int) bool {
		return allowedDocs[i] >= s.offset
	})
	limit := s.offset + s.documentCount
	last := sort.Search(len(allowedDocs), func(i int) bool {
		return allowedDocs[i] >= limit
	})
	keys := make([]uint64, 0, last-first)
	for _, globalDoc := range allowedDocs[first:last] {
		localOffset := globalDoc - s.offset
		localDoc := uint32(localOffset)
		if s.deleted != nil && s.deleted.Contains(localDoc) {
			continue
		}
		key := sort.Search(len(s.payload.DocIDs), func(i int) bool {
			return s.payload.DocIDs[i] >= localDoc
		})
		for key < len(s.payload.DocIDs) && s.payload.DocIDs[key] == localDoc {
			// sort.Search returns a non-negative slice index.
			keys = append(keys, uint64(key+1))
			key++
		}
	}
	return keys, true
}

func embeddedDocumentID(snapshot *index.Snapshot, globalDoc uint64) (Identifier, error) {
	recordVectorStoredIDRead()
	var id Identifier
	if err := snapshot.VisitStoredFields(globalDoc, func(field string, value []byte) bool {
		if field == _idField {
			id = Identifier(string(value))
			return false
		}
		return true
	}); err != nil {
		return "", err
	}
	return id, nil
}

var _ VectorBackend = (*EmbeddedUSearchVectorBackend)(nil)
var _ segment.VectorSegmentBackend = (*EmbeddedUSearchVectorBackend)(nil)
var _ VectorCandidateSearcher = (*embeddedUSearchVectorIndex)(nil)
var _ vectorDocumentCandidateSearcher = (*embeddedUSearchVectorIndex)(nil)
