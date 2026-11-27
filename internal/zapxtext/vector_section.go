package zap

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/RoaringBitmap/roaring/v2"

	"github.com/fy0/bluge/internal/blugeidx"
	blugeseg "github.com/fy0/bluge/segment"
)

const vectorPayloadVersion uint64 = 1

var ErrVectorPayloadNotFound = errors.New("vector payload not found")

type vectorIndexSection struct{}

type vectorIndexOpaque struct {
	backend   blugeseg.VectorSegmentBackend
	fieldsMap map[string]uint16
	records   map[string][]blugeseg.VectorRecord
	addrs     map[int]uint64
}

func (v *vectorIndexSection) Process(map[int]resetable, uint32, *blugeidx.Field, uint16) {}

func (v *vectorIndexSection) ProcessVector(opaque map[int]resetable, docNum uint32,
	vector *blugeidx.Vector, _ uint16) {
	op := getVectorIndexOpaque(opaque)
	op.records[vector.Name()] = append(op.records[vector.Name()], blugeseg.VectorRecord{
		DocNum:     docNum,
		Values:     vector.Values(),
		Similarity: vector.Similarity(),
	})
}

func (v *vectorIndexSection) Persist(opaque map[int]resetable, w *CountHashWriter) error {
	op := getVectorIndexOpaque(opaque)
	if op.backend == nil {
		return nil
	}
	fields := make([]string, 0, len(op.records))
	for field := range op.records {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	for _, field := range fields {
		payload, err := op.backend.BuildVectorPayload(field, op.records[field])
		if err != nil {
			return err
		}
		op.addrs[int(op.fieldsMap[field]-1)] = w.Count()
		if err := writeVectorPayload(w, payload); err != nil {
			return err
		}
	}
	return nil
}

func (v *vectorIndexSection) AddrForField(opaque map[int]resetable, fieldID int) uint64 {
	return getVectorIndexOpaque(opaque).addrs[fieldID]
}

func (v *vectorIndexSection) Merge(opaque map[int]resetable, segments []*SegmentBase,
	_ []*roaring.Bitmap, fields []string, newDocNums [][]uint64, w *CountHashWriter, closeCh chan struct{}) error {
	op := getVectorIndexOpaque(opaque)
	if op.backend == nil {
		return nil
	}
	for fieldID, field := range fields {
		if isClosed(closeCh) {
			return errors.New("vector merge canceled")
		}
		inputs, err := vectorMergeInputs(segments, field, newDocNums)
		if err != nil {
			return err
		}
		if len(inputs) == 0 {
			continue
		}
		payload, err := op.backend.MergeVectorPayload(field, inputs)
		if err != nil {
			return err
		}
		if len(payload.DocIDs) == 0 {
			continue
		}
		op.addrs[fieldID] = w.Count()
		if err := writeVectorPayload(w, payload); err != nil {
			return err
		}
	}
	return nil
}

func vectorMergeInputs(segments []*SegmentBase, field string, newDocNums [][]uint64) ([]blugeseg.VectorMergeInput, error) {
	inputs := make([]blugeseg.VectorMergeInput, 0, len(segments))
	for i, sb := range segments {
		payload, err := sb.VectorPayload(field)
		if errors.Is(err, ErrVectorPayloadNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		inputs = append(inputs, blugeseg.VectorMergeInput{Payload: payload, NewDocNums: newDocNums[i]})
	}
	return inputs, nil
}

func (v *vectorIndexSection) InitOpaque(args sectionConfig) resetable {
	op := &vectorIndexOpaque{records: make(map[string][]blugeseg.VectorRecord), addrs: make(map[int]uint64)}
	op.Configure(args)
	return op
}

func getVectorIndexOpaque(opaque map[int]resetable) *vectorIndexOpaque {
	return opaque[SectionVectorIndex].(*vectorIndexOpaque)
}

func (v *vectorIndexOpaque) Reset() error {
	clear(v.records)
	clear(v.addrs)
	v.backend = nil
	v.fieldsMap = nil
	return nil
}

func (v *vectorIndexOpaque) Configure(args sectionConfig) {
	v.backend = args.options.VectorBackend
	v.fieldsMap = args.fieldsMap
}

func writeVectorPayload(w *CountHashWriter, payload blugeseg.VectorPayload) error {
	if err := writeUvarints(w, vectorPayloadVersion, uint64(payload.Dimensions), uint64(len(payload.DocIDs))); err != nil {
		return err
	}
	docBytes := make([]byte, 0, len(payload.DocIDs)*binary.MaxVarintLen32)
	for _, docID := range payload.DocIDs {
		docBytes = binary.AppendUvarint(docBytes, uint64(docID))
	}
	for _, data := range [][]byte{[]byte(payload.Backend), []byte(payload.Similarity), docBytes, payload.Data} {
		if err := writeUvarints(w, uint64(len(data))); err != nil {
			return err
		}
		if _, err := w.Write(data); err != nil {
			return err
		}
	}
	return nil
}

func (sb *SegmentBase) VectorPayload(field string) (blugeseg.VectorPayload, error) {
	fieldID := sb.fieldsMap[field]
	if fieldID == 0 {
		return blugeseg.VectorPayload{}, ErrVectorPayloadNotFound
	}
	addr := sb.fieldsSectionsMap[fieldID-1][SectionVectorIndex]
	if addr == 0 {
		return blugeseg.VectorPayload{}, ErrVectorPayloadNotFound
	}
	if addr >= uint64(len(sb.mem)) {
		return blugeseg.VectorPayload{}, fmt.Errorf("invalid vector offset")
	}
	return readVectorPayload(sb.mem[addr:])
}

type vectorPayloadReader struct {
	data []byte
	pos  int
}

func (r *vectorPayloadReader) read() (uint64, error) {
	value, n := binary.Uvarint(r.data[r.pos:])
	if n <= 0 {
		return 0, fmt.Errorf("invalid vector varint")
	}
	r.pos += n
	return value, nil
}

func (r *vectorPayloadReader) readBytes() ([]byte, error) {
	size, err := r.read()
	if err != nil {
		return nil, err
	}
	remaining := r.data[r.pos:]
	if size > uint64(len(remaining)) {
		return nil, fmt.Errorf("invalid vector length")
	}
	data := remaining[:size]
	r.pos += len(data)
	return data, nil
}

func readVectorPayload(data []byte) (blugeseg.VectorPayload, error) {
	r := vectorPayloadReader{data: data}
	var header [3]uint64
	for i := range header {
		value, err := r.read()
		if err != nil {
			return blugeseg.VectorPayload{}, err
		}
		header[i] = value
	}
	dimensions := header[1]
	if header[0] != vectorPayloadVersion || dimensions == 0 || dimensions > math.MaxUint32 {
		return blugeseg.VectorPayload{}, fmt.Errorf("invalid vector header")
	}
	var parts [4][]byte
	for i := range parts {
		value, err := r.readBytes()
		if err != nil {
			return blugeseg.VectorPayload{}, err
		}
		parts[i] = value
	}
	docIDs, err := readVectorDocIDs(parts[2], header[2])
	if err != nil {
		return blugeseg.VectorPayload{}, err
	}
	return blugeseg.VectorPayload{
		Backend:    string(parts[0]),
		Dimensions: uint32(dimensions),
		Similarity: string(parts[1]),
		DocIDs:     docIDs,
		Data:       parts[3],
	}, nil
}

func readVectorDocIDs(data []byte, count uint64) ([]uint32, error) {
	if count > uint64(len(data)) {
		return nil, fmt.Errorf("invalid vector document count")
	}
	docIDs := make([]uint32, count)
	for i := range docIDs {
		id, n := binary.Uvarint(data)
		if n <= 0 || id > math.MaxUint32 {
			return nil, fmt.Errorf("invalid vector document mapping")
		}
		docIDs[i] = uint32(id)
		data = data[n:]
	}
	if len(data) != 0 {
		return nil, fmt.Errorf("trailing vector document mapping")
	}
	return docIDs, nil
}
