package cluster

import (
	"math"

	"github.com/mrueg/constellation/internal/embed"
)

// silhouette estimates clustering quality on a sample of points using cosine
// distance. Values run from -1 (points sit closer to a neighbouring cluster)
// through 0 (clusters overlap) to 1 (tight, well-separated clusters).
func silhouette(sp *embed.Space, r *Result, sample []int) float64 {
	if r == nil || r.K < 2 || len(sample) < 2 {
		return math.Inf(-1)
	}
	sum, count := 0.0, 0
	for _, i := range sample {
		ci := r.Assign[i]
		if ci < 0 {
			continue
		}
		totals := make([]float64, r.K)
		counts := make([]int, r.K)
		for _, j := range sample {
			if i == j {
				continue
			}
			cj := r.Assign[j]
			if cj < 0 {
				continue
			}
			totals[cj] += 1 - sp.Rows[i].DotSparse(sp.Rows[j])
			counts[cj]++
		}
		if counts[ci] == 0 {
			continue // a singleton in the sample tells us nothing
		}
		a := totals[ci] / float64(counts[ci])
		b := math.Inf(1)
		for c := 0; c < r.K; c++ {
			if c == ci || counts[c] == 0 {
				continue
			}
			if v := totals[c] / float64(counts[c]); v < b {
				b = v
			}
		}
		if math.IsInf(b, 1) {
			continue
		}
		denom := math.Max(a, b)
		if denom > 0 {
			sum += (b - a) / denom
			count++
		}
	}
	if count == 0 {
		return math.Inf(-1)
	}
	return sum / float64(count)
}
