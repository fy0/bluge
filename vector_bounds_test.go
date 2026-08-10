// Copyright (c) 2026 The Bluge Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package bluge

import (
	"math"
	"strconv"
	"testing"
)

func TestCheckedVectorUint32(t *testing.T) {
	if _, err := checkedVectorUint32(-1, "test value"); err == nil {
		t.Fatal("expected negative value to fail")
	}
	value, err := checkedVectorUint32(42, "test value")
	if err != nil {
		t.Fatal(err)
	}
	if value != 42 {
		t.Fatalf("expected 42, got %d", value)
	}

	oversized := uint64(math.MaxUint32) + 1
	if strconv.IntSize == 64 {
		if _, err := checkedVectorUint32(int(oversized), "test value"); err == nil {
			t.Fatal("expected value above uint32 to fail")
		}
	}
}

func TestCheckedVectorUint32ToInt(t *testing.T) {
	value, err := checkedVectorUint32ToInt(42, "test value")
	if err != nil {
		t.Fatal(err)
	}
	if value != 42 {
		t.Fatalf("expected 42, got %d", value)
	}

	if strconv.IntSize == 32 {
		if _, err := checkedVectorUint32ToInt(math.MaxUint32, "test value"); err == nil {
			t.Fatal("expected value above platform int to fail")
		}
	}
}
