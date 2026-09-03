package cluster

import (
	"sort"

	"github.com/mrueg/constellation/internal/embed"
)

// Consensus clusters the agreement between several runs rather than trusting
// any one of them.
//
// A single k-means run is a local optimum reached from a random start, and on
// this data the choice of start matters more than it looks: measured on a real
// account, only eighteen of twenty-nine categories survived a change of seed,
// and the categories that did keep their names exchanged a quarter of their
// members. A taxonomy that reshuffles itself when nothing about the input
// changed is not describing the input.
//
// The method is cluster-based similarity partitioning. Each run is turned into
// an indicator vector — one dimension per cluster of that run, with a
// repository marking the cluster it joined — and the vectors are concatenated.
// The cosine between two repositories in that space is then exactly the
// fraction of runs that put them together, so clustering it groups
// repositories the runs agree about and leaves the contested ones to fall
// where they may. It costs nothing in memory beyond one entry per run per
// repository, which is what makes it affordable on a corpus this size; a full
// co-association matrix would be seventeen million entries.
func Consensus(sp *embed.Space, opt Options, runs int) *Result {
	if runs < 2 || len(sp.Rows) == 0 {
		return Run(sp, opt)
	}

	// How many categories there are is settled once. Repeating the silhouette
	// sweep for every run would spend most of the time re-deriving the same
	// answer, and it is the starting point that is being varied here, not the
	// number of clusters.
	first := Run(sp, opt)
	if first.K == 0 {
		return first
	}
	parts := []*Result{first}
	for i := 1; i < runs; i++ {
		o := withK(opt, first.K)
		o.Seed = opt.Seed + int64(i)*7919
		if res := Run(sp, o); res.K > 0 {
			parts = append(parts, res)
		}
	}
	if len(parts) == 0 {
		return &Result{}
	}
	if len(parts) == 1 {
		return parts[0]
	}

	consensus := Run(indicatorSpace(parts, len(sp.Rows)), withK(opt, first.K))

	// The agreement space says which repositories belong together, but every
	// measurement downstream — cohesion, outlier rejection, the order within a
	// list — is expressed against the vectors the repositories actually have,
	// so the centroids are rebuilt there.
	out := &Result{Assign: consensus.Assign, K: consensus.K, Centroids: make([][]float64, consensus.K)}
	for c := range out.Centroids {
		out.Centroids[c] = make([]float64, sp.Dim)
	}
	recomputeCentroids(sp, out)

	score, assigned := 0.0, 0
	for i, c := range out.Assign {
		if c >= 0 {
			score += sp.Rows[i].Dot(out.Centroids[c])
			assigned++
		}
	}
	if assigned > 0 {
		out.Score = score / float64(assigned)
	}
	return out
}

// indicatorSpace lays every run's clusters end to end and marks, for each
// repository, the one cluster it joined in each run.
func indicatorSpace(parts []*Result, n int) *embed.Space {
	offsets := make([]int, len(parts))
	dim := 0
	for i, p := range parts {
		offsets[i] = dim
		dim += p.K
	}
	rows := make([]embed.Vector, n)
	for i := 0; i < n; i++ {
		idx := make([]int32, 0, len(parts))
		for p, part := range parts {
			if c := part.Assign[i]; c >= 0 {
				// Bounded by the total cluster count across runs — at most
				// --consensus times --max-clusters, a few hundred — so this
				// cannot overflow.
				idx = append(idx, int32(offsets[p]+c)) //nolint:gosec // G115: bounded above
			}
		}
		sort.Slice(idx, func(a, b int) bool { return idx[a] < idx[b] })
		val := make([]float32, len(idx))
		for j := range val {
			val[j] = 1
		}
		rows[i] = embed.Normalize(idx, val)
	}
	return &embed.Space{Dim: dim, Rows: rows, Backend: "consensus"}
}

// withK fixes the cluster count. The runs already agreed roughly on how many
// there are, so the consensus is not searched for again.
func withK(opt Options, k int) Options {
	opt.K = k
	return opt
}
