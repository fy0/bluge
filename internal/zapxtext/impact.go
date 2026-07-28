// Copyright (c) 2026 The Bluge Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package zap

import (
	"encoding/binary"
	"fmt"
	"io"

	blugeseg "github.com/fy0/bluge/segment"
)

const (
	impactBlockSize   = 64
	impactMinPostings = 128
)

type impactCoder struct {
	enabled       bool
	blockDocs     int
	blockEnd      uint64
	previousEnd   uint64
	blocks        uint64
	frontier      [impactBlockSize]blugeseg.Impact
	frontierLen   int
	encodedBlocks []byte
	raw           []byte
	buf           [binary.MaxVarintLen64]byte
}

func (c *impactCoder) Reset(cardinality uint64) {
	c.enabled = cardinality >= impactMinPostings
	c.blockDocs = 0
	c.blockEnd = 0
	c.previousEnd = 0
	c.blocks = 0
	c.frontierLen = 0
	c.encodedBlocks = c.encodedBlocks[:0]
}

func (c *impactCoder) Add(docNum, freq, norm uint64) {
	if !c.enabled {
		return
	}
	c.blockEnd = docNum
	c.blockDocs++
	c.addToFrontier(freq, norm)
	if c.blockDocs == impactBlockSize {
		c.flushBlock()
	}
}

func (c *impactCoder) addToFrontier(freq, norm uint64) {
	frontier := c.frontier[:c.frontierLen]
	for _, impact := range frontier {
		if impact.Frequency >= freq && impact.Norm <= norm {
			return
		}
	}

	kept := 0
	for _, impact := range frontier {
		if freq >= impact.Frequency && norm <= impact.Norm {
			continue
		}
		c.frontier[kept] = impact
		kept++
	}
	c.frontier[kept] = blugeseg.Impact{Frequency: freq, Norm: norm}
	c.frontierLen = kept + 1
}

func (c *impactCoder) flushBlock() {
	if c.blockDocs == 0 {
		return
	}
	frontier := c.frontier[:c.frontierLen]
	// The frontier has at most 64 entries; insertion sort avoids a per-block
	// interface allocation from sort.Slice.
	for i := 1; i < len(frontier); i++ {
		impact := frontier[i]
		j := i
		for j > 0 && impactLess(impact, frontier[j-1]) {
			frontier[j] = frontier[j-1]
			j--
		}
		frontier[j] = impact
	}
	c.writeUvarint(c.blockEnd - c.previousEnd)
	c.writeUvarint(uint64(len(frontier)))
	for _, impact := range frontier {
		c.writeUvarint(impact.Frequency)
		c.writeUvarint(impact.Norm)
	}
	c.previousEnd = c.blockEnd
	c.blocks++
	c.blockDocs = 0
	c.frontierLen = 0
}

func impactLess(a, b blugeseg.Impact) bool {
	if a.Frequency == b.Frequency {
		return a.Norm < b.Norm
	}
	return a.Frequency < b.Frequency
}

func (c *impactCoder) writeUvarint(value uint64) {
	c.encodedBlocks = binary.AppendUvarint(c.encodedBlocks, value)
}

func (c *impactCoder) writeAt(w *FileWriter) (uint64, error) {
	if !c.enabled {
		return 0, nil
	}
	c.flushBlock()

	blockCountLen := binary.PutUvarint(c.buf[:], c.blocks)
	if w.processor == nil {
		// The default writer can stream the header and retained buffer directly.
		offset := uint64(w.Count())
		n := binary.PutUvarint(c.buf[:], uint64(blockCountLen+len(c.encodedBlocks)))
		if _, err := w.Write(c.buf[:n]); err != nil {
			return 0, err
		}
		n = binary.PutUvarint(c.buf[:], c.blocks)
		if _, err := w.Write(c.buf[:n]); err != nil {
			return 0, err
		}
		if _, err := w.Write(c.encodedBlocks); err != nil {
			return 0, err
		}
		return offset, nil
	}

	c.raw = binary.AppendUvarint(c.raw[:0], c.blocks)
	c.raw = append(c.raw, c.encodedBlocks...)
	encoded := w.process(c.raw)

	offset := uint64(w.Count())
	n := binary.PutUvarint(c.buf[:], uint64(len(encoded)))
	if _, err := w.Write(c.buf[:n]); err != nil {
		return 0, err
	}
	if _, err := w.Write(encoded); err != nil {
		return 0, err
	}
	return offset, nil
}

type impactDecoder struct {
	data         []byte
	pos          int
	blocksLeft   uint64
	currentEnd   uint64
	currentValid bool
	impacts      []blugeseg.Impact
	initialized  bool
}

func (d *impactDecoder) reset(data []byte) error {
	d.data = data
	d.pos = 0
	d.blocksLeft = 0
	d.currentEnd = 0
	d.currentValid = false
	d.impacts = d.impacts[:0]
	d.initialized = true

	blocks, err := d.readUvarint()
	if err != nil {
		return fmt.Errorf("read impact block count: %w", err)
	}
	d.blocksLeft = blocks
	return nil
}

func (d *impactDecoder) advanceShallow(docNum uint64) (uint64, []blugeseg.Impact, bool, error) {
	for !d.currentValid || d.currentEnd < docNum {
		if d.blocksLeft == 0 {
			return 0, nil, false, nil
		}
		endDelta, err := d.readUvarint()
		if err != nil {
			return 0, nil, false, fmt.Errorf("read impact block end: %w", err)
		}
		count, err := d.readUvarint()
		if err != nil {
			return 0, nil, false, fmt.Errorf("read impact count: %w", err)
		}
		if count == 0 || count > impactBlockSize {
			return 0, nil, false, fmt.Errorf("invalid impact count %d", count)
		}
		if cap(d.impacts) < int(count) {
			d.impacts = make([]blugeseg.Impact, int(count))
		} else {
			d.impacts = d.impacts[:int(count)]
		}
		for i := range d.impacts {
			freq, err := d.readUvarint()
			if err != nil {
				return 0, nil, false, fmt.Errorf("read impact frequency: %w", err)
			}
			norm, err := d.readUvarint()
			if err != nil {
				return 0, nil, false, fmt.Errorf("read impact norm: %w", err)
			}
			d.impacts[i] = blugeseg.Impact{Frequency: freq, Norm: norm}
		}
		if ^uint64(0)-d.currentEnd < endDelta {
			return 0, nil, false, fmt.Errorf("impact block end overflow")
		}
		d.currentEnd += endDelta
		d.currentValid = true
		d.blocksLeft--
	}
	return d.currentEnd, d.impacts, true, nil
}

func (d *impactDecoder) readUvarint() (uint64, error) {
	if d.pos >= len(d.data) {
		return 0, io.ErrUnexpectedEOF
	}
	value, n := binary.Uvarint(d.data[d.pos:])
	if n <= 0 {
		return 0, io.ErrUnexpectedEOF
	}
	d.pos += n
	return value, nil
}
