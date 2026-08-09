package zap

// VectorRecord is one vector value in a segment-local document number.
type VectorRecord struct {
	DocNum     uint32
	Values     []float32
	Similarity string
}

// VectorPayload is the backend-neutral envelope stored in a segment. The
// Data field is opaque to zapx; the backend owns its serialization format.
type VectorPayload struct {
	Backend    string
	Dimensions uint32
	Similarity string
	DocIDs     []uint32
	Data       []byte
}

type VectorMergeInput struct {
	Payload    VectorPayload
	NewDocNums []uint64
}

// VectorSegmentBackend is implemented by a native vector backend that can
// build and merge one serialized index per text segment.
type VectorSegmentBackend interface {
	SegmentVectorBackend()
	Name() string
	BuildVectorPayload(field string, records []VectorRecord) (VectorPayload, error)
	MergeVectorPayload(field string, inputs []VectorMergeInput) (VectorPayload, error)
}

func vectorSegmentBackend(config map[string]interface{}) VectorSegmentBackend {
	if config == nil {
		return nil
	}
	backend, _ := config["vector_backend"].(VectorSegmentBackend)
	return backend
}
