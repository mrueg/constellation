package cluster

import (
	"math"

	"github.com/mrueg/constellation/internal/embed"
)

// FromMembership rebuilds a Result from memberships decided earlier — the
// categories of a plan written days ago, re-embedded in a fresh space — so
// that new repositories can be filed into them without re-deriving the
// categories themselves.
//
// Centroids are derived from every member, including the secondary
// memberships, because a plan file does not distinguish them: it records the
// repositories in each category, not which category claimed them first.
func FromMembership(sp *embed.Space, assign []int, also [][]int, k int) *Result {
	r := &Result{Assign: assign, Also: also, K: k, Centroids: make([][]float64, k)}
	counts := make([]int, k)
	for c := range r.Centroids {
		r.Centroids[c] = make([]float64, sp.Dim)
	}
	for i, row := range sp.Rows {
		for _, c := range r.memberships(i) {
			counts[c]++
			for j, ix := range row.Idx {
				r.Centroids[c][ix] += float64(row.Val[j])
			}
		}
	}
	for c := range r.Centroids {
		if counts[c] > 0 {
			normalize(r.Centroids[c])
		}
	}
	r.Score = meanFit(sp, r)
	return r
}

// memberships lists every cluster repository i belongs to.
func (r *Result) memberships(i int) []int {
	c := -1
	if i < len(r.Assign) {
		c = r.Assign[i]
	}
	var out []int
	if c >= 0 && c < r.K {
		out = append(out, c)
	}
	if i < len(r.Also) {
		for _, a := range r.Also[i] {
			if a >= 0 && a < r.K && a != c {
				out = append(out, a)
			}
		}
	}
	return out
}

// PlaceOptions govern where a repository that has no category yet may go. They
// are the same two tests refinement applies, so that a repository arriving
// later is judged by the bar its neighbours had to clear.
type PlaceOptions struct {
	MinSimilarity float64
	OutlierSigmas float64
	// MultiLists and MultiRatio mirror Soften, applied to the newcomer alone.
	MultiLists int
	MultiRatio float64
}

// Place files the given repositories into the clusters that already exist and
// reports how many it placed. Clusters are not moved, split or re-centred, and
// no repository outside todo is touched — which is the point: an incremental
// run must not reshuffle lists a person has already reviewed and applied.
//
// A repository that fits nothing well enough is left unassigned rather than
// pushed into the nearest category, exactly as a full run would leave it.
func Place(sp *embed.Space, r *Result, todo []int, opt PlaceOptions) int {
	if r == nil || r.K == 0 || len(todo) == 0 {
		return 0
	}
	if len(r.Also) != len(r.Assign) {
		also := make([][]int, len(r.Assign))
		copy(also, r.Also)
		r.Also = also
	}
	floors, _ := fitFloors(sp, r, opt.MinSimilarity, opt.OutlierSigmas)

	placed := 0
	for _, i := range todo {
		if i < 0 || i >= len(sp.Rows) {
			continue
		}
		row := sp.Rows[i]
		best, bestSim := -1, math.Inf(-1)
		for c := range r.Centroids {
			if sim := row.Dot(r.Centroids[c]); sim > bestSim {
				best, bestSim = c, sim
			}
		}
		if best < 0 || bestSim < floors[best] {
			continue
		}
		r.Assign[i] = best
		softenOne(sp, r, i, opt.MultiRatio, opt.MultiLists)
		placed++
	}
	return placed
}
