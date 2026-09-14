//go:build (windows || linux || darwin) && (amd64 || arm64)

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

// EmbeddedUSearchVectorBackend stores one serialized USearch index in every
// zapx segment. It is deliberately separate from USearchVectorBackend, whose
// global sidecar remains useful as a performance baseline.
type EmbeddedUSearchVectorBackend struct {
	libraryPath string
	options     USearchVectorOptions
}

func NewEmbeddedUSearchVectorBackend() *EmbeddedUSearchVectorBackend {
	return &EmbeddedUSearchVectorBackend{options: defaultUSearchVectorOptions()}
}

func (b *EmbeddedUSearchVectorBackend) Name() string { return vectorBackendUSearchName }

func (b *EmbeddedUSearchVectorBackend) SegmentVectorBackend() {}

func (b *EmbeddedUSearchVectorBackend) WithLibraryPath(path string) *EmbeddedUSearchVectorBackend {
	if b == nil {
		return nil
	}
	copy := *b
	copy.libraryPath = path
	return &copy
}

func (b *EmbeddedUSearchVectorBackend) Open(Config) (VectorIndex, error) {
	return nil, errors.New("embedded usearch backend is opened from an index snapshot")
}

func (b *EmbeddedUSearchVectorBackend) ValidateVectorChanges(changes []VectorChange) error {
	for _, change := range changes {
		if !change.Delete {
			if err := validateVectorChange(change); err != nil {
				return err
			}
		}
	}
	return nil
}

func (b *EmbeddedUSearchVectorBackend) BuildVectorPayload(field string,
	records []zapxtext.VectorRecord) (zapxtext.VectorPayload, error) {
	if len(records) == 0 {
		return zapxtext.VectorPayload{}, fmt.Errorf("field %q has no vectors", field)
	}
	options, err := b.options.normalized()
	if err != nil {
		return zapxtext.VectorPayload{}, err
	}
	api, err := loadUSearchAPI(b.libraryPath)
	if err != nil {
		return zapxtext.VectorPayload{}, err
	}
	dimensions := len(records[0].Values)
	similarity := VectorSimilarity(records[0].Similarity)
	if similarity == "" {
		similarity = VectorCosine
	}
	if dimensions == 0 {
		return zapxtext.VectorPayload{}, ErrVectorInvalidDimension
	}
	encodedDimensions, err := checkedVectorUint32(dimensions, "embedded vector dimensions")
	if err != nil {
		return zapxtext.VectorPayload{}, err
	}
	for _, record := range records {
		if len(record.Values) != dimensions || VectorSimilarity(record.Similarity) != similarity {
			return zapxtext.VectorPayload{}, fmt.Errorf("field %q has inconsistent vector shape", field)
		}
		for _, value := range record.Values {
			if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
				return zapxtext.VectorPayload{}, ErrVectorInvalidValue
			}
		}
	}
	handle := api.create(uintptr(dimensions), usearchMetric(similarity),
		uintptr(options.Connectivity), uintptr(options.ExpansionAdd), uintptr(options.ExpansionSearch))
	if handle == nil {
		return zapxtext.VectorPayload{}, fmt.Errorf("create embedded usearch field %q failed", field)
	}
	defer api.destroy(handle)
	if status := api.reserve(handle, len(records)); status != 0 {
		return zapxtext.VectorPayload{}, usearchNativeStatus(api, handle, status, "reserve")
	}
	adder := newUSearchBatchAdder(api, handle, dimensions)
	for key, record := range records {
		if err := adder.Add(uint64(key+1), record.Values); err != nil {
			return zapxtext.VectorPayload{}, err
		}
	}
	if err := adder.Flush(); err != nil {
		return zapxtext.VectorPayload{}, err
	}
	data, err := serializeUSearchIndex(api, handle)
	if err != nil {
		return zapxtext.VectorPayload{}, fmt.Errorf("serialize embedded usearch field %q: %w", field, err)
	}
	docIDs := make([]uint32, len(records))
	for i, record := range records {
		docIDs[i] = record.DocNum
	}
	return zapxtext.VectorPayload{
		Backend:    b.Name(),
		Dimensions: encodedDimensions,
		Similarity: string(similarity),
		DocIDs:     docIDs,
		Data:       data,
	}, nil
}

func (b *EmbeddedUSearchVectorBackend) MergeVectorPayload(field string,
	inputs []zapxtext.VectorMergeInput) (zapxtext.VectorPayload, error) {
	if len(inputs) == 0 {
		return zapxtext.VectorPayload{}, fmt.Errorf("field %q has no merge inputs", field)
	}
	options, err := b.options.normalized()
	if err != nil {
		return zapxtext.VectorPayload{}, err
	}
	api, err := loadUSearchAPI(b.libraryPath)
	if err != nil {
		return zapxtext.VectorPayload{}, err
	}
	first := inputs[0].Payload
	dimensions := int(first.Dimensions)
	similarity := VectorSimilarity(first.Similarity)
	if dimensions <= 0 || similarity == "" {
		return zapxtext.VectorPayload{}, fmt.Errorf("field %q has invalid merge metadata", field)
	}
	var live int
	for _, input := range inputs {
		if int(input.Payload.Dimensions) != dimensions || input.Payload.Similarity != first.Similarity {
			return zapxtext.VectorPayload{}, fmt.Errorf("field %q has incompatible merge inputs", field)
		}
		for _, docID := range input.Payload.DocIDs {
			if int(docID) < len(input.NewDocNums) && input.NewDocNums[docID] != math.MaxUint64 {
				live++
			}
		}
	}
	if live == 0 {
		return zapxtext.VectorPayload{}, nil
	}
	target := api.create(uintptr(dimensions), usearchMetric(similarity),
		uintptr(options.Connectivity), uintptr(options.ExpansionAdd), uintptr(options.ExpansionSearch))
	if target == nil {
		return zapxtext.VectorPayload{}, fmt.Errorf("create merged embedded usearch field %q failed", field)
	}
	defer api.destroy(target)
	if status := api.reserve(target, live); status != 0 {
		return zapxtext.VectorPayload{}, usearchNativeStatus(api, target, status, "reserve merge")
	}
	docIDs := make([]uint32, 0, live)
	key := uint64(1)
	adder := newUSearchBatchAdder(api, target, dimensions)
	for inputID, input := range inputs {
		source := api.openBuffer(input.Payload.Data)
		if source == nil {
			return zapxtext.VectorPayload{}, fmt.Errorf("open vector payload %d for field %q failed", inputID, field)
		}
		for sourceKey, docID := range input.Payload.DocIDs {
			if int(docID) >= len(input.NewDocNums) {
				api.destroy(source)
				return zapxtext.VectorPayload{}, fmt.Errorf("field %q has invalid source doc mapping", field)
			}
			newDocID := input.NewDocNums[docID]
			if newDocID == math.MaxUint64 {
				continue
			}
			if newDocID > math.MaxUint32 {
				api.destroy(source)
				return zapxtext.VectorPayload{}, fmt.Errorf(
					"field %q has out-of-range merged document number %d", field, newDocID)
			}
			vector := make([]float32, dimensions)
			if status := api.get(source, uint64(sourceKey+1), vector); status != 0 {
				nativeErr := usearchNativeStatus(api, source, status, "get merge")
				api.destroy(source)
				return zapxtext.VectorPayload{}, nativeErr
			}
			if err := adder.Add(key, vector); err != nil {
				api.destroy(source)
				return zapxtext.VectorPayload{}, err
			}
			key++
			docIDs = append(docIDs, uint32(newDocID))
		}
		api.destroy(source)
	}
	if err := adder.Flush(); err != nil {
		return zapxtext.VectorPayload{}, err
	}
	data, err := serializeUSearchIndex(api, target)
	if err != nil {
		return zapxtext.VectorPayload{}, fmt.Errorf("serialize merged embedded field %q: %w", field, err)
	}
	return zapxtext.VectorPayload{
		Backend:    b.Name(),
		Dimensions: first.Dimensions,
		Similarity: string(similarity),
		DocIDs:     docIDs,
		Data:       data,
	}, nil
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
	payload       zapxtext.VectorPayload
	offset        uint64
	documentCount uint64
	deleted       *roaring.Bitmap
}

type snapshotSegmentAccess interface {
	Segment() segment.Segment
	Deleted() *roaring.Bitmap
}

func (b *EmbeddedUSearchVectorBackend) OpenSnapshot(snapshot *index.Snapshot) (VectorIndex, error) {
	if snapshot == nil {
		return nil, errors.New("embedded usearch backend requires an index snapshot")
	}
	api, err := loadUSearchAPI(b.libraryPath)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrVectorUnsupported, err)
	}
	rv := &embeddedUSearchVectorIndex{
		snapshot: snapshot,
		fields:   make(map[string][]embeddedUSearchSegment),
	}
	offset := uint64(0)
	for _, candidate := range snapshot.Segments() {
		access, ok := candidate.(snapshotSegmentAccess)
		if !ok {
			return nil, fmt.Errorf("index snapshot does not expose segment access")
		}
		inner := access.Segment()
		if unwrap, ok := inner.(interface {
			UnderlyingSegment() segment.Segment
		}); ok {
			inner = unwrap.UnderlyingSegment()
		}
		if inner == nil {
			return nil, fmt.Errorf("index snapshot contains a nil segment")
		}
		payloadReader, ok := inner.(interface {
			VectorPayload(string) (zapxtext.VectorPayload, error)
		})
		if !ok {
			offset += inner.Count()
			continue
		}
		for _, field := range inner.Fields() {
			payload, payloadErr := payloadReader.VectorPayload(field)
			if errors.Is(payloadErr, zapxtext.ErrVectorPayloadNotFound) {
				continue
			}
			if payloadErr != nil {
				_ = rv.Close()
				return nil, payloadErr
			}
			if payload.Backend != b.Name() {
				_ = rv.Close()
				return nil, fmt.Errorf("unsupported embedded vector backend %q", payload.Backend)
			}
			if !sort.SliceIsSorted(payload.DocIDs, func(i, j int) bool {
				return payload.DocIDs[i] < payload.DocIDs[j]
			}) {
				_ = rv.Close()
				return nil, fmt.Errorf("embedded vector field %q has unsorted document mapping", field)
			}
			handle := api.openBuffer(payload.Data)
			if handle == nil {
				_ = rv.Close()
				return nil, fmt.Errorf("open embedded vector field %q failed", field)
			}
			rv.fields[field] = append(rv.fields[field], embeddedUSearchSegment{
				api:           api,
				handle:        handle,
				payload:       payload,
				offset:        offset,
				documentCount: inner.Count(),
				deleted:       access.Deleted(),
			})
		}
		offset += inner.Count()
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
	return e.searchCandidates(field, query, k, nil, nil)
}

func (e *embeddedUSearchVectorIndex) SearchCandidates(field string, query []float32,
	k int, allowed map[Identifier]struct{}) ([]VectorHit, error) {
	return e.searchCandidates(field, query, k, allowed, nil)
}

func (e *embeddedUSearchVectorIndex) searchDocumentCandidates(field string, query []float32,
	k int, allowed []uint64) ([]VectorHit, error) {
	return e.searchCandidates(field, query, k, nil, allowed)
}

// embeddedVectorCandidate is an intermediate search result. The stored
// identifier is resolved only when the global ranking actually needs it, so a
// search does not pay for the identifiers of candidates that the global
// truncation discards.
type embeddedVectorCandidate struct {
	globalDoc uint64
	score     float64
	id        Identifier
	idSet     bool
}

func (e *embeddedUSearchVectorIndex) searchCandidates(field string, query []float32,
	k int, allowedIDs map[Identifier]struct{}, allowedDocs []uint64) ([]VectorHit, error) {
	if k <= 0 {
		return nil, ErrVectorInvalidK
	}
	if len(query) == 0 {
		return nil, ErrVectorInvalidDimension
	}
	for _, value := range query {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return nil, ErrVectorInvalidValue
		}
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.closed {
		return nil, errors.New("embedded usearch vector index is closed")
	}
	segments := e.fields[field]
	if len(segments) == 0 {
		return nil, fmt.Errorf("%w: %q", ErrVectorFieldNotFound, field)
	}
	for _, segment := range segments {
		if int(segment.payload.Dimensions) != len(query) {
			return nil, fmt.Errorf("%w: field %q has %d dimensions, got %d",
				ErrVectorInvalidDimension, field, segment.payload.Dimensions, len(query))
		}
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

// collectCandidates runs the per-segment native search. Every segment
// contributes its own top-k, which is enough for the union to contain the
// global top-k. Identifiers are resolved here only when the filter itself is
// expressed in terms of identifiers.
func (e *embeddedUSearchVectorIndex) collectCandidates(segments []embeddedUSearchSegment,
	query []float32, k int, allowedIDs map[Identifier]struct{},
	allowedDocs []uint64) ([]embeddedVectorCandidate, error) {
	candidates := make([]embeddedVectorCandidate, 0, len(segments)*k)
	for _, segment := range segments {
		count := k
		if allowedIDs != nil {
			count = len(segment.payload.DocIDs)
		}
		if count == 0 {
			continue
		}
		allowedKeys, filtered := segment.allowedKeys(allowedDocs)
		if filtered && len(allowedKeys) == 0 {
			continue
		}
		keys := make([]uint64, count)
		distances := make([]float32, count)
		var status int32
		var resultCount int
		if filtered {
			status, resultCount = segment.api.searchFiltered(segment.handle, query, count,
				allowedKeys, keys, distances)
		} else {
			status, resultCount = segment.api.search(segment.handle, query, count, keys, distances)
		}
		if err := usearchNativeStatus(segment.api, segment.handle, status, "search"); err != nil {
			return nil, err
		}
		if resultCount < 0 || resultCount > len(keys) || resultCount > len(distances) {
			return nil, fmt.Errorf("embedded usearch returned invalid result count %d", resultCount)
		}
		similarity := VectorSimilarity(segment.payload.Similarity)
		for i := 0; i < resultCount; i++ {
			if keys[i] == 0 || keys[i]-1 >= uint64(len(segment.payload.DocIDs)) {
				return nil, fmt.Errorf("embedded usearch returned invalid key %d", keys[i])
			}
			localDoc := segment.payload.DocIDs[keys[i]-1]
			if segment.deleted != nil && segment.deleted.Contains(localDoc) {
				continue
			}
			candidate := embeddedVectorCandidate{
				globalDoc: segment.offset + uint64(localDoc),
				score:     usearchScore(similarity, float64(distances[i])),
			}
			if allowedIDs != nil {
				id, err := embeddedDocumentID(e.snapshot, candidate.globalDoc)
				if err != nil {
					return nil, err
				}
				if _, ok := allowedIDs[id]; !ok {
					continue
				}
				candidate.id = id
				candidate.idSet = true
			}
			candidates = append(candidates, candidate)
		}
	}
	return candidates, nil
}

// rankCandidates reproduces the documented ordering (score descending, then
// identifier ascending) while resolving as few stored identifiers as possible.
//
// When more candidates arrive than k, the k-th score decides everything:
// candidates scoring above it are returned whatever their identifiers are,
// candidates scoring exactly it compete for the remaining slots, and
// candidates scoring below it can never reach the result. Ordering by score
// alone needs no identifier, so the split is computed before any identifier is
// read and the candidates below the cutoff are dropped unread.
//
// The identifier comparison inside the tied group is the documented one.
// Document numbers cannot stand in for identifiers there.
//
// The sorts are not stable, which is safe here: two candidates only compare
// equal when they carry the same score and the same identifier, and such
// candidates produce indistinguishable VectorHit entries. Candidates sharing a
// score but differing in identifier are ordered by identifier, and the split
// by score does not depend on the order inside a score group.
func (e *embeddedUSearchVectorIndex) rankCandidates(candidates []embeddedVectorCandidate,
	k int) ([]VectorHit, error) {
	if len(candidates) <= k {
		// No candidate can be dropped, so there is no split to compute and
		// every candidate needs its identifier. This is the single-segment
		// shape of a search.
		if err := e.resolveCandidateIDs(candidates); err != nil {
			return nil, err
		}
		slices.SortFunc(candidates, compareScoreThenID)
		return vectorHits(candidates, nil), nil
	}
	// The k-th score decides everything: candidates above it are returned
	// whatever their identifiers are, candidates on it compete for the
	// remaining slots, and candidates below it can never reach the result.
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
	// Candidates after tiedEnd are dropped without an identifier read.
	slices.SortFunc(selected, compareScoreThenID)
	slices.SortFunc(tied, func(a, b embeddedVectorCandidate) int {
		return cmp.Compare(a.id, b.id)
	})
	keep := k - len(selected)
	if keep > len(tied) {
		keep = len(tied)
	}
	if keep < 0 {
		keep = 0
	}
	return vectorHits(selected, tied[:keep]), nil
}

func compareScoreThenID(a, b embeddedVectorCandidate) int {
	if byScore := cmp.Compare(b.score, a.score); byScore != 0 {
		return byScore
	}
	return cmp.Compare(a.id, b.id)
}

// vectorHits converts candidate groups that are already in result order into
// the returned hits. tail is empty when nothing was truncated.
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
		for key, localDoc := range s.payload.DocIDs {
			if !s.deleted.Contains(localDoc) {
				keys = append(keys, uint64(key+1))
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
		if localOffset > math.MaxUint32 {
			continue
		}
		localDoc := uint32(localOffset)
		if s.deleted != nil && s.deleted.Contains(localDoc) {
			continue
		}
		key := sort.Search(len(s.payload.DocIDs), func(i int) bool {
			return s.payload.DocIDs[i] >= localDoc
		})
		for key < len(s.payload.DocIDs) && s.payload.DocIDs[key] == localDoc {
			// sort.Search returns a non-negative slice index.
			keys = append(keys, uint64(key+1)) //nolint:gosec
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
	if id == "" {
		return "", fmt.Errorf("embedded vector result has no stored identifier")
	}
	return id, nil
}

var _ VectorBackend = (*EmbeddedUSearchVectorBackend)(nil)
var _ zapxtext.VectorSegmentBackend = (*EmbeddedUSearchVectorBackend)(nil)
var _ VectorCandidateSearcher = (*embeddedUSearchVectorIndex)(nil)
var _ vectorDocumentCandidateSearcher = (*embeddedUSearchVectorIndex)(nil)
