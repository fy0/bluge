//go:build (windows || linux || darwin) && (amd64 || arm64)

package bluge

import (
	"errors"
	"fmt"
	"math"
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

func (b *EmbeddedUSearchVectorBackend) Name() string { return "usearch" }

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
	for key, record := range records {
		if status := api.add(handle, uint64(key+1), record.Values); status != 0 {
			return zapxtext.VectorPayload{}, usearchNativeStatus(api, handle, status, "add")
		}
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
		Dimensions: uint32(dimensions),
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
			vector := make([]float32, dimensions)
			if status := api.get(source, uint64(sourceKey+1), vector); status != 0 {
				nativeErr := usearchNativeStatus(api, source, status, "get merge")
				api.destroy(source)
				return zapxtext.VectorPayload{}, nativeErr
			}
			if status := api.add(target, key, vector); status != 0 {
				api.destroy(source)
				return zapxtext.VectorPayload{}, usearchNativeStatus(api, target, status, "add merge")
			}
			key++
			docIDs = append(docIDs, uint32(newDocID))
		}
		api.destroy(source)
	}
	data, err := serializeUSearchIndex(api, target)
	if err != nil {
		return zapxtext.VectorPayload{}, fmt.Errorf("serialize merged embedded field %q: %w", field, err)
	}
	return zapxtext.VectorPayload{
		Backend:    b.Name(),
		Dimensions: uint32(dimensions),
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

func usearchNativeStatus(api usearchNativeAPI, handle unsafe.Pointer,
	status int32, operation string) error {
	if status == 0 {
		return nil
	}
	message := "native operation failed"
	if api != nil {
		message = api.errorMessage(handle)
		if message == "" {
			message = "native operation failed"
		}
	}
	return fmt.Errorf("usearch %s: %s", operation, message)
}

type embeddedUSearchVectorIndex struct {
	mu       sync.RWMutex
	snapshot *index.Snapshot
	fields   map[string][]embeddedUSearchSegment
	closed   bool
}

type embeddedUSearchSegment struct {
	api     usearchNativeAPI
	handle  unsafe.Pointer
	payload zapxtext.VectorPayload
	offset  uint64
	deleted *roaring.Bitmap
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
			handle := api.openBuffer(payload.Data)
			if handle == nil {
				_ = rv.Close()
				return nil, fmt.Errorf("open embedded vector field %q failed", field)
			}
			rv.fields[field] = append(rv.fields[field], embeddedUSearchSegment{
				api:     api,
				handle:  handle,
				payload: payload,
				offset:  offset,
				deleted: access.Deleted(),
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
	return e.searchCandidates(field, query, k, nil)
}

func (e *embeddedUSearchVectorIndex) SearchCandidates(field string, query []float32,
	k int, allowed map[Identifier]struct{}) ([]VectorHit, error) {
	return e.searchCandidates(field, query, k, allowed)
}

func (e *embeddedUSearchVectorIndex) searchCandidates(field string, query []float32,
	k int, allowed map[Identifier]struct{}) ([]VectorHit, error) {
	if k <= 0 {
		return nil, ErrVectorInvalidK
	}
	if len(query) == 0 {
		return nil, ErrVectorInvalidDimension
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
	hits := make([]VectorHit, 0, len(segments)*k)
	for _, segment := range segments {
		if int(segment.payload.Dimensions) != len(query) {
			return nil, fmt.Errorf("%w: field %q has %d dimensions, got %d",
				ErrVectorInvalidDimension, field, segment.payload.Dimensions, len(query))
		}
		count := k
		if allowed != nil || (segment.deleted != nil && !segment.deleted.IsEmpty()) {
			count = len(segment.payload.DocIDs)
		}
		if count == 0 {
			continue
		}
		keys := make([]uint64, count)
		distances := make([]float32, count)
		status, resultCount := segment.api.search(segment.handle, query, count, keys, distances)
		if err := usearchNativeStatus(segment.api, segment.handle, status, "search"); err != nil {
			return nil, err
		}
		for i := 0; i < resultCount; i++ {
			if keys[i] == 0 || keys[i]-1 >= uint64(len(segment.payload.DocIDs)) {
				return nil, fmt.Errorf("embedded usearch returned invalid key %d", keys[i])
			}
			localDoc := segment.payload.DocIDs[keys[i]-1]
			if segment.deleted != nil && segment.deleted.Contains(localDoc) {
				continue
			}
			globalDoc := segment.offset + uint64(localDoc)
			id, err := embeddedDocumentID(e.snapshot, globalDoc)
			if err != nil {
				return nil, err
			}
			if allowed != nil {
				if _, ok := allowed[id]; !ok {
					continue
				}
			}
			hits = append(hits, VectorHit{
				ID:    id,
				Score: usearchScore(VectorSimilarity(segment.payload.Similarity), float64(distances[i])),
			})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score == hits[j].Score {
			return hits[i].ID < hits[j].ID
		}
		return hits[i].Score > hits[j].Score
	})
	if len(hits) > k {
		hits = hits[:k]
	}
	return hits, nil
}

func embeddedDocumentID(snapshot *index.Snapshot, globalDoc uint64) (Identifier, error) {
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
