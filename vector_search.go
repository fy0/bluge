// Copyright (c) 2026 The Bluge Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package bluge

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/fy0/bluge/search"
)

var (
	ErrVectorInvalidRequest = errors.New("vector search request is invalid")
	ErrHybridInvalidRequest = errors.New("hybrid search request is invalid")
)

// VectorSearchRequest is the fluent form of a vector search. The existing
// Reader.VectorSearch method remains available for callers that do not need
// candidate tuning.
type VectorSearchRequest struct {
	Field      string
	Vector     []float32
	K          int
	Candidates int
	Filter     Query
}

// NewVectorSearchRequest creates a vector request with a default result size
// of ten. The vector is copied so callers can reuse their input buffer.
func NewVectorSearchRequest(field string, vector []float32) *VectorSearchRequest {
	return &VectorSearchRequest{
		Field:  field,
		Vector: append([]float32(nil), vector...),
		K:      10,
	}
}

func (r *VectorSearchRequest) SetK(k int) *VectorSearchRequest {
	if r != nil {
		r.K = k
	}
	return r
}

func (r *VectorSearchRequest) SetCandidates(candidates int) *VectorSearchRequest {
	if r != nil {
		r.Candidates = candidates
	}
	return r
}

func (r *VectorSearchRequest) SetFilter(filter Query) *VectorSearchRequest {
	if r != nil {
		r.Filter = filter
	}
	return r
}

func (r VectorSearchRequest) normalized() (VectorSearchRequest, error) {
	if r.Field == "" {
		return VectorSearchRequest{}, fmt.Errorf("%w: field is empty", ErrVectorInvalidRequest)
	}
	if r.K <= 0 {
		return VectorSearchRequest{}, ErrVectorInvalidK
	}
	if r.Candidates < 0 {
		return VectorSearchRequest{}, fmt.Errorf("%w: candidates cannot be negative", ErrVectorInvalidRequest)
	}
	if r.Candidates < r.K {
		r.Candidates = r.K
	}
	return r, nil
}

// SearchVectorRequest executes a vector request and applies the final K after
// candidate retrieval. Candidates is useful for hybrid search and for ANN
// recall tuning; zero means exactly K candidates.
func (r *Reader) SearchVectorRequest(ctx context.Context,
	request *VectorSearchRequest) ([]VectorHit, error) {
	if request == nil {
		return nil, fmt.Errorf("%w: request is nil", ErrVectorInvalidRequest)
	}
	normalized, err := request.normalized()
	if err != nil {
		return nil, err
	}
	hits, err := r.VectorSearch(ctx, normalized.Field, normalized.Vector,
		normalized.Candidates, normalized.Filter)
	if err != nil {
		return nil, err
	}
	if len(hits) > normalized.K {
		hits = hits[:normalized.K]
	}
	return hits, nil
}

// HybridFusionMode controls how text and vector candidates are combined.
type HybridFusionMode string

const (
	// HybridFusionWeighted normalizes each candidate list to [0, 1] and
	// combines the lists using TextWeight and VectorWeight.
	HybridFusionWeighted HybridFusionMode = "weighted"
	// HybridFusionRRF uses reciprocal rank fusion and is robust when text and
	// vector scores have different distributions.
	HybridFusionRRF HybridFusionMode = "rrf"
)

// HybridSearchRequest combines a Bluge Query with a vector request. Either
// side may be omitted, which makes this type useful for gradual migration from
// text-only or vector-only search as well as true hybrid search.
type HybridSearchRequest struct {
	TextQuery Query
	Vector    *VectorSearchRequest
	Filter    Query

	K                int
	TextCandidates   int
	VectorCandidates int

	Fusion       HybridFusionMode
	TextWeight   float64
	VectorWeight float64
	RRFK         int
}

// NewHybridSearchRequest creates a hybrid request from the two independent
// query objects. Both objects are optional, but at least one must be present
// when the request is executed.
func NewHybridSearchRequest(text Query, vector *VectorSearchRequest) *HybridSearchRequest {
	return &HybridSearchRequest{
		TextQuery:    text,
		Vector:       vector,
		K:            10,
		Fusion:       HybridFusionWeighted,
		TextWeight:   1,
		VectorWeight: 1,
		RRFK:         60,
	}
}

func (r *HybridSearchRequest) SetK(k int) *HybridSearchRequest {
	if r != nil {
		r.K = k
	}
	return r
}

func (r *HybridSearchRequest) SetFilter(filter Query) *HybridSearchRequest {
	if r != nil {
		r.Filter = filter
	}
	return r
}

func (r *HybridSearchRequest) SetTextCandidates(candidates int) *HybridSearchRequest {
	if r != nil {
		r.TextCandidates = candidates
	}
	return r
}

func (r *HybridSearchRequest) SetVectorCandidates(candidates int) *HybridSearchRequest {
	if r != nil {
		r.VectorCandidates = candidates
	}
	return r
}

func (r *HybridSearchRequest) SetFusion(mode HybridFusionMode) *HybridSearchRequest {
	if r != nil {
		r.Fusion = mode
	}
	return r
}

func (r *HybridSearchRequest) SetWeights(text, vector float64) *HybridSearchRequest {
	if r != nil {
		r.TextWeight = text
		r.VectorWeight = vector
	}
	return r
}

func (r *HybridSearchRequest) SetRRFK(k int) *HybridSearchRequest {
	if r != nil {
		r.RRFK = k
	}
	return r
}

func (r HybridSearchRequest) normalized() (HybridSearchRequest, error) {
	if r.K <= 0 {
		return HybridSearchRequest{}, fmt.Errorf("%w: k must be greater than zero", ErrHybridInvalidRequest)
	}
	if r.TextQuery == nil && r.Vector == nil {
		return HybridSearchRequest{}, fmt.Errorf("%w: text query and vector query are both empty", ErrHybridInvalidRequest)
	}
	if r.TextCandidates < 0 || r.VectorCandidates < 0 {
		return HybridSearchRequest{}, fmt.Errorf("%w: candidate counts cannot be negative", ErrHybridInvalidRequest)
	}
	if r.Fusion == "" {
		r.Fusion = HybridFusionWeighted
	}
	if r.Fusion != HybridFusionWeighted && r.Fusion != HybridFusionRRF {
		return HybridSearchRequest{}, fmt.Errorf("%w: unsupported fusion mode %q", ErrHybridInvalidRequest, r.Fusion)
	}
	if math.IsNaN(r.TextWeight) || math.IsInf(r.TextWeight, 0) || r.TextWeight < 0 ||
		math.IsNaN(r.VectorWeight) || math.IsInf(r.VectorWeight, 0) || r.VectorWeight < 0 ||
		r.TextWeight+r.VectorWeight == 0 {
		return HybridSearchRequest{}, fmt.Errorf("%w: weights must be finite and at least one must be positive", ErrHybridInvalidRequest)
	}
	if r.RRFK <= 0 {
		r.RRFK = 60
	}
	return r, nil
}

// HybridHit contains the raw scores from both branches and the fused score.
// A rank of zero means that the document was absent from that branch's
// candidate set.
type HybridHit struct {
	ID          Identifier
	Score       float64
	TextScore   float64
	VectorScore float64
	TextRank    int
	VectorRank  int
}

// HybridSearch executes text and vector retrieval independently, then fuses
// their candidate sets in Go. The native backend is deliberately unaware of
// Bluge queries and filters.
func (r *Reader) HybridSearch(ctx context.Context,
	request *HybridSearchRequest) ([]HybridHit, error) {
	if request == nil {
		return nil, fmt.Errorf("%w: request is nil", ErrHybridInvalidRequest)
	}
	normalized, err := request.normalized()
	if err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}

	textCandidates := make(map[Identifier]hybridCandidate)
	if normalized.TextQuery != nil {
		count := normalized.TextCandidates
		if count == 0 {
			count = hybridDefaultCandidates(normalized.K)
		}
		textQuery := combineHybridQueries(normalized.TextQuery, normalized.Filter)
		iterator, searchErr := r.Search(ctx, NewTopNSearch(count, textQuery))
		if searchErr != nil {
			return nil, searchErr
		}
		for rank := 1; ; rank++ {
			match, nextErr := iterator.Next()
			if nextErr != nil {
				return nil, nextErr
			}
			if match == nil {
				break
			}
			id, idErr := documentMatchIdentifier(match)
			if idErr != nil {
				return nil, idErr
			}
			textCandidates[id] = hybridCandidate{
				id:        id,
				textScore: match.Score,
				textRank:  rank,
				hasText:   true,
			}
		}
	}

	vectorCandidates := make(map[Identifier]hybridCandidate)
	if normalized.Vector != nil {
		vectorRequest := *normalized.Vector
		vectorRequest.Filter = combineHybridQueries(vectorRequest.Filter, normalized.Filter)
		count := normalized.VectorCandidates
		if count == 0 {
			count = vectorRequest.Candidates
		}
		if count == 0 {
			count = hybridDefaultCandidates(normalized.K)
		}
		vectorRequest.K = count
		vectorRequest.Candidates = count
		hits, searchErr := r.SearchVectorRequest(ctx, &vectorRequest)
		if searchErr != nil {
			return nil, searchErr
		}
		for rank, hit := range hits {
			vectorCandidates[hit.ID] = hybridCandidate{
				id:          hit.ID,
				vectorScore: hit.Score,
				vectorRank:  rank + 1,
				hasVector:   true,
			}
		}
	}

	all := make(map[Identifier]hybridCandidate, len(textCandidates)+len(vectorCandidates))
	for id, candidate := range textCandidates {
		all[id] = candidate
	}
	for id, candidate := range vectorCandidates {
		merged := all[id]
		if merged.id == "" {
			merged.id = id
		}
		if candidate.hasVector {
			merged.vectorScore = candidate.vectorScore
			merged.vectorRank = candidate.vectorRank
			merged.hasVector = true
		}
		all[id] = merged
	}

	textNormalized := normalizeHybridScores(textCandidates, true)
	vectorNormalized := normalizeHybridScores(vectorCandidates, false)
	hits := make([]HybridHit, 0, len(all))
	for id, candidate := range all {
		var score float64
		switch normalized.Fusion {
		case HybridFusionRRF:
			if candidate.textRank > 0 {
				score += normalized.TextWeight /
					float64(normalized.RRFK+candidate.textRank)
			}
			if candidate.vectorRank > 0 {
				score += normalized.VectorWeight /
					float64(normalized.RRFK+candidate.vectorRank)
			}
		default:
			score = normalized.TextWeight*textNormalized[id] +
				normalized.VectorWeight*vectorNormalized[id]
		}
		hits = append(hits, HybridHit{
			ID:          id,
			Score:       score,
			TextScore:   candidate.textScore,
			VectorScore: candidate.vectorScore,
			TextRank:    candidate.textRank,
			VectorRank:  candidate.vectorRank,
		})
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score == hits[j].Score {
			return hits[i].ID < hits[j].ID
		}
		return hits[i].Score > hits[j].Score
	})
	if len(hits) > normalized.K {
		hits = hits[:normalized.K]
	}
	return hits, nil
}

type hybridCandidate struct {
	id          Identifier
	textScore   float64
	vectorScore float64
	textRank    int
	vectorRank  int
	hasText     bool
	hasVector   bool
}

func hybridDefaultCandidates(k int) int {
	if k > math.MaxInt/10 {
		return math.MaxInt
	}
	count := k * 10
	if count < 100 {
		return 100
	}
	return count
}

func combineHybridQueries(first, second Query) Query {
	if first == nil {
		return second
	}
	if second == nil {
		return first
	}
	return NewBooleanQuery().AddMust(first, second)
}

func documentMatchIdentifier(match *search.DocumentMatch) (Identifier, error) {
	var id Identifier
	if err := match.VisitStoredFields(func(field string, value []byte) bool {
		if field == _idField {
			id = Identifier(string(value))
			return false
		}
		return true
	}); err != nil {
		return "", err
	}
	if id == "" {
		return "", fmt.Errorf("%w: text result has no stored identifier", ErrHybridInvalidRequest)
	}
	return id, nil
}

func normalizeHybridScores(candidates map[Identifier]hybridCandidate,
	text bool) map[Identifier]float64 {
	result := make(map[Identifier]float64, len(candidates))
	if len(candidates) == 0 {
		return result
	}
	minScore := math.Inf(1)
	maxScore := math.Inf(-1)
	for _, candidate := range candidates {
		score := candidate.vectorScore
		if text {
			score = candidate.textScore
		}
		if score < minScore {
			minScore = score
		}
		if score > maxScore {
			maxScore = score
		}
	}
	if maxScore == minScore {
		for id := range candidates {
			result[id] = 1
		}
		return result
	}
	for id, candidate := range candidates {
		score := candidate.vectorScore
		if text {
			score = candidate.textScore
		}
		result[id] = (score - minScore) / (maxScore - minScore)
	}
	return result
}
