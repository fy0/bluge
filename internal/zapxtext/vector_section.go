package zap

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/RoaringBitmap/roaring/v2"

	"github.com/fy0/bluge/internal/blugeidx"
)

const (
	vectorPayloadVersion uint64 = 1
	vectorFieldsMapKey          = "fieldsMap"
)

var ErrVectorPayloadNotFound = errors.New("vector payload was not found")

type vectorIndexSection struct{}

type vectorIndexOpaque struct {
	backend   VectorSegmentBackend
	fieldsMap map[string]uint16
	records   map[string][]VectorRecord
	addrs     map[int]int
}

func init() {
	registerSegmentSection(SectionVectorIndex, &vectorIndexSection{})
}

func (v *vectorIndexSection) Process(map[int]resetable, uint32, *blugeidx.Field, uint16) {}

func (v *vectorIndexSection) ProcessVector(opaque map[int]resetable, docNum uint32,
	vector *blugeidx.Vector, fieldID uint16) {
	io := getVectorIndexOpaque(opaque)
	if io.backend == nil || vector == nil || fieldID == math.MaxUint16 {
		return
	}
	io.records[vector.Name()] = append(io.records[vector.Name()], VectorRecord{
		DocNum:     docNum,
		Values:     append([]float32(nil), vector.Values()...),
		Similarity: vector.Similarity(),
	})
}

func (v *vectorIndexSection) Persist(opaque map[int]resetable, w *FileWriter) error {
	io := getVectorIndexOpaque(opaque)
	if io.backend == nil {
		return nil
	}
	fields := make([]string, 0, len(io.records))
	for field := range io.records {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	for _, field := range fields {
		fieldID, ok := io.fieldsMap[field]
		if !ok || len(io.records[field]) == 0 {
			continue
		}
		payload, err := io.backend.BuildVectorPayload(field, io.records[field])
		if err != nil {
			return fmt.Errorf("build embedded vector field %q: %w", field, err)
		}
		io.addrs[int(fieldID-1)] = w.Count()
		if err := writeVectorPayload(w, payload); err != nil {
			return fmt.Errorf("write embedded vector field %q: %w", field, err)
		}
	}
	return nil
}

func (v *vectorIndexSection) AddrForField(opaque map[int]resetable, fieldID int) int {
	return getVectorIndexOpaque(opaque).addrs[fieldID]
}

func (v *vectorIndexSection) Merge(opaque map[int]resetable, segments []*SegmentBase,
	drops []*roaring.Bitmap, fieldsInv []string, newDocNumsIn [][]uint64,
	w *FileWriter, closeCh chan struct{}) error {
	io := getVectorIndexOpaque(opaque)
	if io.backend == nil {
		return nil
	}
	for fieldID, field := range fieldsInv {
		if isClosed(closeCh) {
			return errors.New("embedded vector merge canceled")
		}
		inputs := make([]VectorMergeInput, 0, len(segments))
		for segmentID, sb := range segments {
			payload, err := sb.VectorPayload(field)
			if errors.Is(err, ErrVectorPayloadNotFound) {
				continue
			}
			if err != nil {
				return fmt.Errorf("read embedded vector field %q from segment %d: %w",
					field, segmentID, err)
			}
			inputs = append(inputs, VectorMergeInput{
				Payload:    payload,
				NewDocNums: newDocNumsIn[segmentID],
			})
		}
		if len(inputs) == 0 {
			continue
		}
		payload, err := io.backend.MergeVectorPayload(field, inputs)
		if err != nil {
			return fmt.Errorf("merge embedded vector field %q: %w", field, err)
		}
		if len(payload.DocIDs) == 0 || len(payload.Data) == 0 {
			continue
		}
		io.addrs[fieldID] = w.Count()
		if err := writeVectorPayload(w, payload); err != nil {
			return fmt.Errorf("write merged embedded vector field %q: %w", field, err)
		}
	}
	return nil
}

func (v *vectorIndexSection) InitOpaque(args map[string]interface{}) resetable {
	config, _ := args["config"].(map[string]interface{})
	return &vectorIndexOpaque{
		backend:   vectorSegmentBackend(config),
		fieldsMap: copyVectorFieldsMap(args[vectorFieldsMapKey]),
		records:   make(map[string][]VectorRecord),
		addrs:     make(map[int]int),
	}
}

func getVectorIndexOpaque(opaque map[int]resetable) *vectorIndexOpaque {
	if value, ok := opaque[SectionVectorIndex].(*vectorIndexOpaque); ok {
		return value
	}
	return &vectorIndexOpaque{
		records: make(map[string][]VectorRecord),
		addrs:   make(map[int]int),
	}
}

func (v *vectorIndexOpaque) Reset() error {
	clear(v.records)
	clear(v.addrs)
	return nil
}

func (v *vectorIndexOpaque) Set(key string, value interface{}) {
	switch key {
	case "config":
		config, _ := value.(map[string]interface{})
		v.backend = vectorSegmentBackend(config)
	case vectorFieldsMapKey:
		v.fieldsMap = copyVectorFieldsMap(value)
	}
}

func copyVectorFieldsMap(value interface{}) map[string]uint16 {
	fields, _ := value.(map[string]uint16)
	if fields == nil {
		return nil
	}
	rv := make(map[string]uint16, len(fields))
	for field, id := range fields {
		rv[field] = id
	}
	return rv
}

func writeVectorPayload(w *FileWriter, payload VectorPayload) error {
	if payload.Backend == "" || len(payload.DocIDs) == 0 || len(payload.Data) == 0 {
		return fmt.Errorf("empty embedded vector payload")
	}
	if int(payload.Dimensions) <= 0 {
		return fmt.Errorf("embedded vector payload has invalid dimensions %d", payload.Dimensions)
	}
	if len(payload.DocIDs) > math.MaxInt32 {
		return fmt.Errorf("embedded vector payload has too many vectors: %d", len(payload.DocIDs))
	}

	if _, err := writeUvarints(w, math.MaxUint64, math.MaxUint64,
		vectorPayloadVersion, uint64(payload.Dimensions), uint64(len(payload.DocIDs))); err != nil {
		return err
	}
	if err := writeVectorBytes(w, []byte(payload.Backend)); err != nil {
		return err
	}
	if err := writeVectorBytes(w, []byte(payload.Similarity)); err != nil {
		return err
	}

	docBytes := make([]byte, 0, len(payload.DocIDs)*binary.MaxVarintLen64)
	var buf [binary.MaxVarintLen64]byte
	for _, docID := range payload.DocIDs {
		n := binary.PutUvarint(buf[:], uint64(docID))
		docBytes = append(docBytes, buf[:n]...)
	}
	if err := writeVectorBytes(w, docBytes); err != nil {
		return err
	}
	return writeVectorBytes(w, payload.Data)
}

func writeVectorBytes(w *FileWriter, data []byte) error {
	processed := w.process(data)
	if _, err := writeUvarints(w, uint64(len(processed))); err != nil {
		return err
	}
	_, err := w.Write(processed)
	return err
}

// VectorPayload returns the opaque native payload for one field. It is kept on
// SegmentBase so a higher-level backend can open the native index without
// making zapx depend on that backend's runtime.
func (sb *SegmentBase) VectorPayload(field string) (VectorPayload, error) {
	fieldIDPlusOne, ok := sb.fieldsMap[field]
	if !ok {
		return VectorPayload{}, ErrVectorPayloadNotFound
	}
	addr := sb.fieldsSectionsMap[fieldIDPlusOne-1][SectionVectorIndex]
	if addr == 0 {
		return VectorPayload{}, ErrVectorPayloadNotFound
	}
	pos, err := vectorUint64ToInt(addr, "offset")
	if err != nil {
		return VectorPayload{}, err
	}
	return readVectorPayload(sb, pos)
}

func vectorUint64ToInt(value uint64, label string) (int, error) {
	if value > uint64(^uint(0)>>1) {
		return 0, fmt.Errorf("vector payload %s %d exceeds platform limits", label, value)
	}
	return int(value), nil
}

func readVectorPayload(sb *SegmentBase, pos int) (VectorPayload, error) {
	read := func() (uint64, error) {
		if pos < 0 || pos >= len(sb.mem) {
			return 0, fmt.Errorf("vector payload offset %d is out of range", pos)
		}
		value, n := binary.Uvarint(sb.mem[pos:])
		if n <= 0 {
			return 0, fmt.Errorf("invalid vector payload varint at %d", pos)
		}
		pos += n
		return value, nil
	}
	readBytes := func() ([]byte, error) {
		length, err := read()
		if err != nil {
			return nil, err
		}
		size, err := vectorUint64ToInt(length, "length")
		if err != nil {
			return nil, err
		}
		if size > len(sb.mem)-pos {
			return nil, fmt.Errorf("vector payload length %d exceeds segment", length)
		}
		data, err := sb.fileReader.process(sb.mem[pos : pos+size])
		if err != nil {
			return nil, err
		}
		pos += size
		return data, nil
	}

	if _, err := read(); err != nil { // invalid doc-values start
		return VectorPayload{}, err
	}
	if _, err := read(); err != nil { // invalid doc-values end
		return VectorPayload{}, err
	}
	version, err := read()
	if err != nil {
		return VectorPayload{}, err
	}
	if version != vectorPayloadVersion {
		return VectorPayload{}, fmt.Errorf("unsupported vector payload version %d", version)
	}
	dimensions, err := read()
	if err != nil {
		return VectorPayload{}, err
	}
	count, err := read()
	if err != nil {
		return VectorPayload{}, err
	}
	if dimensions == 0 || dimensions > math.MaxUint32 {
		return VectorPayload{}, fmt.Errorf("vector payload has invalid dimensions %d", dimensions)
	}
	backend, err := readBytes()
	if err != nil {
		return VectorPayload{}, err
	}
	similarity, err := readBytes()
	if err != nil {
		return VectorPayload{}, err
	}
	docBytes, err := readBytes()
	if err != nil {
		return VectorPayload{}, err
	}
	data, err := readBytes()
	if err != nil {
		return VectorPayload{}, err
	}

	documentCount, err := vectorUint64ToInt(count, "document count")
	if err != nil {
		return VectorPayload{}, err
	}
	if documentCount > len(sb.mem) {
		return VectorPayload{}, fmt.Errorf("vector payload has unreasonable document count %d", count)
	}
	docIDs := make([]uint32, 0, documentCount)
	docPos := 0
	for docPos < len(docBytes) && uint64(len(docIDs)) < count {
		docID, n := binary.Uvarint(docBytes[docPos:])
		if n <= 0 || docID > math.MaxUint32 {
			return VectorPayload{}, fmt.Errorf("invalid vector document mapping")
		}
		docIDs = append(docIDs, uint32(docID))
		docPos += n
	}
	if uint64(len(docIDs)) != count {
		return VectorPayload{}, fmt.Errorf("vector document mapping has %d entries, expected %d",
			len(docIDs), count)
	}
	if docPos != len(docBytes) {
		return VectorPayload{}, fmt.Errorf("vector document mapping has trailing bytes")
	}

	return VectorPayload{
		Backend:    string(backend),
		Dimensions: uint32(dimensions),
		Similarity: string(similarity),
		DocIDs:     docIDs,
		Data:       data,
	}, nil
}
