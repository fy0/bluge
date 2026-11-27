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

type VectorSearchRequest struct {
	Field      string
	Vector     []float32
	K          int
	Candidates int
	Filter     Query
}

func NewVectorSearchRequest(field string, vector []float32) *VectorSearchRequest {
	return &VectorSearchRequest{
		Field:  field,
		Vector: append([]float32(nil), vector...),
		K:      10,
	}
}

func (r *VectorSearchRequest) SetK(k int) *VectorSearchRequest {
	r.K = k
	return r
}

func (r *VectorSearchRequest) SetCandidates(candidates int) *VectorSearchRequest {
	r.Candidates = candidates
	return r
}

func (r *VectorSearchRequest) SetFilter(filter Query) *VectorSearchRequest {
	r.Filter = filter
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

func (r *Reader) SearchVectorRequest(ctx context.Context,
	request *VectorSearchRequest) ([]VectorHit, error) {
	if request == nil {
		return nil, fmt.Errorf("%w: request is nil", ErrVectorInvalidRequest)
	}
	normalized, err := request.normalized()
	if err != nil {
		return nil, err
	}
	if err := validateVectorValues(normalized.Vector); err != nil {
		return nil, err
	}
	hits, err := r.vectorSearch(ctx, normalized.Field, normalized.Vector,
		normalized.Candidates, normalized.Filter)
	if err != nil {
		return nil, err
	}
	if len(hits) > normalized.K {
		hits = hits[:normalized.K]
	}
	return hits, nil
}

type HybridFusionMode string

const (
	HybridFusionWeighted HybridFusionMode = "weighted"
	HybridFusionRRF      HybridFusionMode = "rrf"
)

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
	r.K = k
	return r
}

func (r *HybridSearchRequest) SetFilter(filter Query) *HybridSearchRequest {
	r.Filter = filter
	return r
}

func (r *HybridSearchRequest) SetTextCandidates(candidates int) *HybridSearchRequest {
	r.TextCandidates = candidates
	return r
}

func (r *HybridSearchRequest) SetVectorCandidates(candidates int) *HybridSearchRequest {
	r.VectorCandidates = candidates
	return r
}

func (r *HybridSearchRequest) SetFusion(mode HybridFusionMode) *HybridSearchRequest {
	r.Fusion = mode
	return r
}

func (r *HybridSearchRequest) SetWeights(text, vector float64) *HybridSearchRequest {
	r.TextWeight = text
	r.VectorWeight = vector
	return r
}

func (r *HybridSearchRequest) SetRRFK(k int) *HybridSearchRequest {
	r.RRFK = k
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
	if !validHybridWeights(r.TextWeight, r.VectorWeight) {
		return HybridSearchRequest{}, fmt.Errorf("%w: weights must be finite and at least one must be positive", ErrHybridInvalidRequest)
	}
	if r.RRFK <= 0 {
		r.RRFK = 60
	}
	return r, nil
}

type HybridHit struct {
	ID          Identifier
	Score       float64
	TextScore   float64
	VectorScore float64
	TextRank    int
	VectorRank  int
}

func (r *Reader) HybridSearch(ctx context.Context, request *HybridSearchRequest) ([]HybridHit, error) {
	if request == nil {
		return nil, fmt.Errorf("%w: nil request", ErrHybridInvalidRequest)
	}
	normalized, err := request.normalized()
	if err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	text, err := r.hybridTextCandidates(ctx, normalized)
	if err != nil {
		return nil, err
	}
	vector, err := r.hybridVectorCandidates(ctx, normalized)
	if err != nil {
		return nil, err
	}
	return fuseHybridCandidates(normalized, text, vector), nil
}

func (r *Reader) hybridTextCandidates(ctx context.Context,
	request HybridSearchRequest) (map[Identifier]hybridCandidate, error) {
	candidates := make(map[Identifier]hybridCandidate)
	if request.TextQuery == nil {
		return candidates, nil
	}
	count := request.TextCandidates
	if count == 0 {
		count = hybridDefaultCandidates(request.K)
	}
	query := combineHybridQueries(request.TextQuery, request.Filter)
	iterator, err := r.Search(ctx, NewTopNSearch(count, query))
	if err != nil {
		return nil, err
	}
	for rank := 1; ; rank++ {
		match, err := iterator.Next()
		if err != nil {
			return nil, err
		}
		if match == nil {
			return candidates, nil
		}
		id, err := documentMatchIdentifier(match)
		if err != nil {
			return nil, err
		}
		candidates[id] = hybridCandidate{textScore: match.Score, textRank: rank}
	}
}

func (r *Reader) hybridVectorCandidates(ctx context.Context,
	request HybridSearchRequest) (map[Identifier]hybridCandidate, error) {
	candidates := make(map[Identifier]hybridCandidate)
	if request.Vector == nil {
		return candidates, nil
	}
	vector := *request.Vector
	vector.Filter = combineHybridQueries(vector.Filter, request.Filter)
	count := request.VectorCandidates
	if count == 0 {
		count = vector.Candidates
	}
	if count == 0 {
		count = hybridDefaultCandidates(request.K)
	}
	vector.K, vector.Candidates = count, count
	hits, err := r.SearchVectorRequest(ctx, &vector)
	if err != nil {
		return nil, err
	}
	for rank, hit := range hits {
		candidates[hit.ID] = hybridCandidate{vectorScore: hit.Score, vectorRank: rank + 1}
	}
	return candidates, nil
}

func fuseHybridCandidates(request HybridSearchRequest,
	text, vector map[Identifier]hybridCandidate) []HybridHit {
	var textScores, vectorScores map[Identifier]float64
	if request.Fusion == HybridFusionWeighted {
		textScores = normalizeHybridScores(text, true)
		vectorScores = normalizeHybridScores(vector, false)
	}
	for id, candidate := range vector {
		merged := text[id]
		merged.vectorScore, merged.vectorRank = candidate.vectorScore, candidate.vectorRank
		text[id] = merged
	}
	hits := make([]HybridHit, 0, len(text))
	for id, candidate := range text {
		score := request.TextWeight*textScores[id] + request.VectorWeight*vectorScores[id]
		if request.Fusion == HybridFusionRRF {
			score = hybridRRFScore(request, candidate)
		}
		hits = append(hits, HybridHit{
			ID: id, Score: score,
			TextScore: candidate.textScore, VectorScore: candidate.vectorScore,
			TextRank: candidate.textRank, VectorRank: candidate.vectorRank,
		})
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score == hits[j].Score {
			return hits[i].ID < hits[j].ID
		}
		return hits[i].Score > hits[j].Score
	})
	return hits[:min(len(hits), request.K)]
}

func hybridRRFScore(request HybridSearchRequest, candidate hybridCandidate) float64 {
	var score float64
	if candidate.textRank > 0 {
		score += request.TextWeight / float64(request.RRFK+candidate.textRank)
	}
	if candidate.vectorRank > 0 {
		score += request.VectorWeight / float64(request.RRFK+candidate.vectorRank)
	}
	return score
}

type hybridCandidate struct {
	textScore   float64
	vectorScore float64
	textRank    int
	vectorRank  int
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

func validHybridWeights(text, vector float64) bool {
	return !math.IsNaN(text) && !math.IsInf(text, 0) && text >= 0 &&
		!math.IsNaN(vector) && !math.IsInf(vector, 0) && vector >= 0 && text+vector > 0
}
