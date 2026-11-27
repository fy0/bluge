package segment

type VectorRecord struct {
	DocNum     uint32
	Values     []float32
	Similarity string
}

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

type VectorSegmentBackend interface {
	BuildVectorPayload(field string, records []VectorRecord) (VectorPayload, error)
	MergeVectorPayload(field string, inputs []VectorMergeInput) (VectorPayload, error)
}

type BuildOptions struct {
	VectorBackend VectorSegmentBackend
}
