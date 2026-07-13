//  Copyright (c) 2020 The Bluge Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// 		http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package aggregations

import (
	"math"
	"strconv"
	"testing"

	"github.com/fy0/bluge/search"
)

const approximateTestSize = 10000

type cardinalityTestSource struct{}

func (cardinalityTestSource) Fields() []string {
	return nil
}

func (cardinalityTestSource) Values(match *search.DocumentMatch) [][]byte {
	value := []byte(strconv.FormatUint(match.Number, 10))
	return [][]byte{value, value}
}

type quantilesTestSource struct{}

func (quantilesTestSource) Fields() []string {
	return nil
}

func (quantilesTestSource) Numbers(match *search.DocumentMatch) []float64 {
	return []float64{float64(match.Number)}
}

func TestCardinalityApproximation(t *testing.T) {
	calculator := newCardinalityTestCalculator()
	if got := calculator.Value(); got != 0 {
		t.Fatalf("expected empty cardinality to be 0, got %f", got)
	}

	consumeRange(calculator, 0, approximateTestSize)
	assertApproximate(t, "cardinality", calculator.Value(), approximateTestSize,
		approximateTestSize*0.02)
}

func TestCardinalityMerge(t *testing.T) {
	left := newCardinalityTestCalculator()
	right := newCardinalityTestCalculator()

	consumeRange(left, 0, 7500)
	consumeRange(right, 5000, approximateTestSize)
	left.Merge(right)

	assertApproximate(t, "merged cardinality", left.Value(), approximateTestSize,
		approximateTestSize*0.02)
}

func TestQuantilesApproximation(t *testing.T) {
	calculator := newQuantilesTestCalculator(t)
	consumeRange(calculator, 0, 1000)

	assertTestQuantiles(t, calculator)

	for _, quantile := range []float64{-0.01, 1.01} {
		if _, err := calculator.Quantile(quantile); err == nil {
			t.Errorf("expected quantile %f to be rejected", quantile)
		}
	}
}

func TestQuantilesMerge(t *testing.T) {
	left := newQuantilesTestCalculator(t)
	right := newQuantilesTestCalculator(t)

	consumeRange(left, 0, 500)
	consumeRange(right, 500, 1000)
	left.Merge(right)

	assertTestQuantiles(t, left)
}

func TestQuantilesCompressionValidation(t *testing.T) {
	metric := Quantiles(quantilesTestSource{})
	if err := metric.SetCompression(0.5); err == nil {
		t.Fatal("expected compression below 1 to be rejected")
	}
	if err := metric.SetCompression(1); err != nil {
		t.Fatalf("expected compression of 1 to remain valid, got %v", err)
	}
}

func newCardinalityTestCalculator() *CardinalityCalculator {
	return Cardinality(cardinalityTestSource{}).Calculator().(*CardinalityCalculator)
}

func newQuantilesTestCalculator(t *testing.T) *QuantilesCalculator {
	t.Helper()
	calculator, ok := Quantiles(quantilesTestSource{}).Calculator().(*QuantilesCalculator)
	if !ok {
		t.Fatal("expected a QuantilesCalculator")
	}
	return calculator
}

func consumeRange(calculator search.Calculator, start, end uint64) {
	for number := start; number < end; number++ {
		calculator.Consume(&search.DocumentMatch{Number: number})
	}
}

func assertTestQuantiles(t *testing.T, calculator *QuantilesCalculator) {
	t.Helper()
	const span = 999.0
	const tolerance = span * 0.02
	for _, test := range []struct {
		quantile float64
		expected float64
	}{
		{quantile: 0, expected: 0},
		{quantile: 0.5, expected: span * 0.5},
		{quantile: 0.9, expected: span * 0.9},
		{quantile: 1, expected: span},
	} {
		got, err := calculator.Quantile(test.quantile)
		if err != nil {
			t.Fatalf("quantile %f failed: %v", test.quantile, err)
		}
		assertApproximate(t, "quantile "+strconv.FormatFloat(test.quantile, 'f', -1, 64),
			got, test.expected, tolerance)
	}
}

func assertApproximate(t *testing.T, name string, got, expected, tolerance float64) {
	t.Helper()
	if math.Abs(got-expected) > tolerance {
		t.Errorf("%s: expected %f +/- %f, got %f", name, expected, tolerance, got)
	}
}
