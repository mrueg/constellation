// Package cluster groups embedded repositories into candidate star lists.
package cluster

import (
	"math"
	"math/rand"
	"sort"

	"github.com/mrueg/constellation/internal/embed"
)

// Result is one clustering of the corpus.
type Result struct {
	// Assign maps a repository index to its cluster index, or -1 when the
	// repository was left unassigned because no cluster fit it.
	Assign []int
	// Also holds any further clusters a repository belongs to. Star lists are
	// many-to-many — a Rust command line tool belongs under both — so an
	// algorithm that can express partial membership records it here. It is nil
	// for the ones that assign each repository to exactly one cluster.
	Also [][]int
	// Centroids are dense, L2-normalized, one per cluster.
	Centroids [][]float64
	// K is the number of clusters, and Score the mean cosine similarity of
	// each repository to its own centroid.
	K     int
	Score float64
}

// Members returns the repository indices belonging to cluster c, in order,
// including those for which it is a secondary membership.
func (r *Result) Members(c int) []int {
	var out []int
	for i, a := range r.Assign {
		if a == c || r.alsoIn(i, c) {
			out = append(out, i)
		}
	}
	return out
}

func (r *Result) alsoIn(i, c int) bool {
	if i >= len(r.Also) {
		return false
	}
	for _, x := range r.Also[i] {
		if x == c {
			return true
		}
	}
	return false
}

// Options control the k-means search.
type Options struct {
	K          int // 0 selects K automatically
	MinK, MaxK int
	Restarts   int
	MaxIter    int
	Seed       int64
	// SilhouetteSample caps how many repositories take part in the silhouette
	// evaluation used to pick K; the full O(n^2) computation is wasteful on
	// large star lists and the sampled estimate ranks K just as well.
	SilhouetteSample int
}

// Run clusters the space, choosing K by sampled silhouette score when
// Options.K is zero.
func Run(sp *embed.Space, opt Options) *Result {
	n := len(sp.Rows)
	if n == 0 {
		return &Result{}
	}
	if opt.K > 0 {
		return best(sp, opt.K, opt)
	}

	minK, maxK := opt.MinK, opt.MaxK
	if minK < 2 {
		minK = 2
	}
	if maxK > n {
		maxK = n
	}
	if maxK < minK {
		maxK = minK
	}

	sample := samplePoints(n, opt.SilhouetteSample, opt.Seed)
	var bestRes *Result
	bestSil := math.Inf(-1)
	for k := minK; k <= maxK; k += kStep(minK, maxK) {
		r := best(sp, k, opt)
		sil := silhouette(sp, r, sample)
		if sil > bestSil {
			bestSil, bestRes = sil, r
		}
	}
	return bestRes
}

// kStep keeps the K sweep to a bounded number of candidate values so that a
// wide range stays affordable on large corpora.
func kStep(minK, maxK int) int {
	const maxCandidates = 14
	span := maxK - minK + 1
	if span <= maxCandidates {
		return 1
	}
	return int(math.Ceil(float64(span) / maxCandidates))
}

func best(sp *embed.Space, k int, opt Options) *Result {
	restarts := opt.Restarts
	if restarts < 1 {
		restarts = 1
	}
	var out *Result
	for r := 0; r < restarts; r++ {
		res := kmeans(sp, k, opt, opt.Seed+int64(r)*7919+int64(k)*104729)
		if out == nil || res.Score > out.Score {
			out = res
		}
	}
	return out
}

func kmeans(sp *embed.Space, k int, opt Options, seed int64) *Result {
	n := len(sp.Rows)
	rng := rand.New(rand.NewSource(seed))
	centroids := kmeansPlusPlus(sp, k, rng)
	assign := make([]int, n)
	for i := range assign {
		assign[i] = -1
	}

	maxIter := opt.MaxIter
	if maxIter < 1 {
		maxIter = 50
	}
	score := 0.0
	for iter := 0; iter < maxIter; iter++ {
		changed := false
		score = 0
		for i, row := range sp.Rows {
			bestC, bestS := 0, math.Inf(-1)
			for c := range centroids {
				if s := row.Dot(centroids[c]); s > bestS {
					bestC, bestS = c, s
				}
			}
			score += bestS
			if assign[i] != bestC {
				assign[i] = bestC
				changed = true
			}
		}
		recentre(sp, assign, centroids, rng)
		if !changed {
			break
		}
	}
	if n > 0 {
		score /= float64(n)
	}
	return &Result{Assign: assign, Centroids: centroids, K: k, Score: score}
}

// kmeansPlusPlus seeds centroids far apart in cosine space, which matters more
// here than in Euclidean k-means because random seeding on sparse text vectors
// routinely produces empty clusters.
func kmeansPlusPlus(sp *embed.Space, k int, rng *rand.Rand) [][]float64 {
	n := len(sp.Rows)
	centroids := make([][]float64, 0, k)
	first := rng.Intn(n)
	centroids = append(centroids, dense(sp.Rows[first], sp.Dim))

	// dist holds 1 - cosine to the nearest chosen centroid.
	dist := make([]float64, n)
	for i := range dist {
		dist[i] = 1 - sp.Rows[i].Dot(centroids[0])
	}
	for len(centroids) < k {
		var total float64
		for _, d := range dist {
			total += d * d
		}
		pick := n - 1
		if total > 0 {
			target := rng.Float64() * total
			acc := 0.0
			for i, d := range dist {
				acc += d * d
				if acc >= target {
					pick = i
					break
				}
			}
		} else {
			pick = rng.Intn(n)
		}
		c := dense(sp.Rows[pick], sp.Dim)
		centroids = append(centroids, c)
		for i := range dist {
			if d := 1 - sp.Rows[i].Dot(c); d < dist[i] {
				dist[i] = d
			}
		}
	}
	return centroids
}

// recentre recomputes each centroid as the normalized mean of its members. An
// emptied cluster is re-seeded onto the repository that currently fits its own
// centroid worst, so K clusters stay K clusters.
func recentre(sp *embed.Space, assign []int, centroids [][]float64, rng *rand.Rand) {
	counts := make([]int, len(centroids))
	for c := range centroids {
		for j := range centroids[c] {
			centroids[c][j] = 0
		}
	}
	for i, row := range sp.Rows {
		c := assign[i]
		if c < 0 {
			continue
		}
		counts[c]++
		for j, ix := range row.Idx {
			centroids[c][ix] += float64(row.Val[j])
		}
	}
	for c := range centroids {
		if counts[c] == 0 {
			copy(centroids[c], dense(sp.Rows[worstFit(sp, assign, centroids, rng)], sp.Dim))
			continue
		}
		normalize(centroids[c])
	}
}

func worstFit(sp *embed.Space, assign []int, centroids [][]float64, rng *rand.Rand) int {
	worst, worstS := rng.Intn(len(sp.Rows)), math.Inf(1)
	for i, row := range sp.Rows {
		c := assign[i]
		if c < 0 {
			continue
		}
		if s := row.Dot(centroids[c]); s < worstS {
			worst, worstS = i, s
		}
	}
	return worst
}

func dense(v embed.Vector, dim int) []float64 {
	out := make([]float64, dim)
	for i, ix := range v.Idx {
		out[ix] = float64(v.Val[i])
	}
	return out
}

func normalize(v []float64) {
	var norm float64
	for _, x := range v {
		norm += x * x
	}
	if norm == 0 {
		return
	}
	inv := 1 / math.Sqrt(norm)
	for i := range v {
		v[i] *= inv
	}
}

func samplePoints(n, cap int, seed int64) []int {
	if cap <= 0 || cap >= n {
		out := make([]int, n)
		for i := range out {
			out[i] = i
		}
		return out
	}
	rng := rand.New(rand.NewSource(seed))
	perm := rng.Perm(n)[:cap]
	sort.Ints(perm)
	return perm
}
