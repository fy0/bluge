// Copyright (c) 2026 The Bluge Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package zap

import "testing"

func TestVectorUint64ToInt(t *testing.T) {
	maxIntValue := int(^uint(0) >> 1)
	maxInt := uint64(^uint(0) >> 1)
	value, err := vectorUint64ToInt(maxInt, "test value")
	if err != nil {
		t.Fatal(err)
	}
	if value != maxIntValue {
		t.Fatalf("expected %d, got %d", maxInt, value)
	}

	if _, err := vectorUint64ToInt(maxInt+1, "test value"); err == nil {
		t.Fatal("expected value above platform int to fail")
	}
}
