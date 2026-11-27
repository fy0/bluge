package zap

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/RoaringBitmap/roaring/v2"

	blugeseg "github.com/fy0/bluge/segment"
)

func LoadBytes(data []byte) (*SegmentBase, error) {
	if len(data) < FooterSize {
		return nil, fmt.Errorf("truncated segment")
	}
	footer := data[len(data)-FooterSize:]
	if version := binary.BigEndian.Uint32(footer[28:]); version != Version {
		return nil, fmt.Errorf("unsupported segment version %d", version)
	}
	return InitSegmentBase(data[:len(data)-FooterSize],
		binary.BigEndian.Uint32(footer[32:]),
		binary.BigEndian.Uint32(footer[24:]),
		binary.BigEndian.Uint64(footer),
		binary.BigEndian.Uint64(footer[8:]),
		binary.BigEndian.Uint64(footer[16:]))
}

func MergeToWriter(segments []*SegmentBase, drops []*roaring.Bitmap, w io.Writer,
	closeCh chan struct{}, options blugeseg.BuildOptions, bufferSize int) ([][]uint64, uint64, error) {
	if bufferSize <= 0 {
		bufferSize = DefaultFileMergerBufferSize
	}
	bw := bufio.NewWriterSize(w, bufferSize)
	cw := NewCountHashWriter(bw)
	newDocNums, numDocs, storedIndexOffset, _, _, sectionsIndexOffset, err :=
		mergeToWriter(segments, drops, DefaultChunkMode, cw, closeCh, options)
	if err != nil {
		return nil, 0, err
	}
	if err = persistFooter(numDocs, storedIndexOffset, sectionsIndexOffset, DefaultChunkMode, cw.Sum32(), cw); err != nil {
		return nil, 0, err
	}
	if err = bw.Flush(); err != nil {
		return nil, 0, err
	}
	return newDocNums, cw.Count(), nil
}
