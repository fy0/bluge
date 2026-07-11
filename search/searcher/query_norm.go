package searcher

import (
	"math"

	"github.com/fy0/bluge/search"
)

func queryNormWeight(searchers []search.Searcher) (float64, bool) {
	var sum float64
	found := false
	for _, child := range searchers {
		if child == nil {
			continue
		}
		weighted, ok := child.(search.QueryNormSearcher)
		if !ok {
			return 0, false
		}
		weight, enabled := weighted.QueryNormWeight()
		if !enabled {
			return 0, false
		}
		sum += weight
		found = true
	}
	return sum, found && sum > 0
}

func setQueryNorm(searchers []search.Searcher, queryNorm float64) {
	for _, child := range searchers {
		if weighted, ok := child.(search.QueryNormSearcher); ok {
			weighted.SetQueryNorm(queryNorm)
		}
	}
}

func normalizeQuery(searchers []search.Searcher) {
	weight, enabled := queryNormWeight(searchers)
	if enabled {
		setQueryNorm(searchers, 1/math.Sqrt(weight))
	}
}
