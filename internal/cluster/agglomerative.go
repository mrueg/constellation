package cluster

import (
	"context"
	"math"
	"sort"

	"gonum.org/v1/gonum/blas"
	"gonum.org/v1/gonum/blas/blas64"

	"github.com/mrueg/constellation/internal/embed"
)

// Agglomerate builds clusters by repeatedly merging the two nearest, using
// Ward linkage.
//
// The reason to prefer it here is that the clustering itself has no random
// element. That is not the same as the pipeline having none: the randomized
// SVD that produces the vectors is seeded, and a different --lsa-seed moves
// the answer more than any documented flag does. What this buys is that the
// ground stops shifting for a fixed seed. Every
// other method in this package starts from a guess and improves it, so the
// guess shows through: measured across seeds, barely half the categories kept
// their names and the ones that did exchanged a fifth of their members. That
// is not a defect of k-means so much as a fact about a corpus with no single
// preferred division — but a method that always returns the same answer at
// least makes the answer reproducible, and lets a change to the input be read
// as a change to the input rather than as a reshuffle.
//
// Ward merges the pair that adds least to the total within-cluster variance,
// which is defined on squared Euclidean distance. The vectors are
// L2-normalized, so that is simply 2(1-cosine) and the geometry is the same
// one every other part of this package uses.
//
// Cost is quadratic in both time and memory — around seventy megabytes for a
// few thousand repositories — which is affordable at this size and would not
// be at a hundred times it.
func Agglomerate(sp *embed.Space, k int) *Result {
	return AgglomerateContext(context.Background(), sp, k)
}

// AgglomerateContext is Agglomerate that stops when ctx is cancelled. The two
// heavy stages — building the pairwise distances and the chain of merges — are
// each quadratic, so a large corpus spends real time here; without a check a
// Ctrl-C during clustering does nothing until it finishes. On cancellation it
// returns whatever it has built, which the caller discards after seeing
// ctx.Err(); the point is to stop wasting CPU, not to produce a usable answer.
func AgglomerateContext(ctx context.Context, sp *embed.Space, k int) *Result {
	n := len(sp.Rows)
	if n == 0 {
		return &Result{}
	}
	if k < 1 {
		k = 1
	}
	if k >= n {
		return singletons(sp)
	}

	dist := pairwise(ctx, sp)
	size := make([]int, n)
	live := make([]bool, n)
	members := make([][]int, n)
	for i := range size {
		size[i], live[i], members[i] = 1, true, []int{i}
	}

	// Nearest-neighbour chain: walk from a cluster to its nearest neighbour
	// until two are each other's nearest, and merge those. It reaches the same
	// dendrogram as repeatedly scanning for the global minimum, without the
	// scan.
	//
	// It does not reach it in the same *order*. The chain descends into one
	// branch and merges deep within it before coming back to shallower pairs
	// elsewhere, so the heights it emits are not ascending — measured on a
	// four-thousand repository corpus, 41% of merges came out below the one
	// before. Stopping after n-k of them therefore cuts the tree wherever the
	// walk happened to be, not at the k tallest joins. So the whole tree is
	// built and the cut is made afterwards, in height order.
	merges := make([]mergeStep, 0, n-1)

	chain := make([]int, 0, n)
	remaining := n
	for remaining > 1 {
		// Cheap next to a merge (which is O(n)), so a per-iteration check adds
		// nothing measurable while making the loop responsive.
		if ctx.Err() != nil {
			break
		}
		if len(chain) == 0 {
			chain = append(chain, firstLive(live))
		}
		a := chain[len(chain)-1]
		b := nearestTo(a, dist, live, n)
		if b < 0 {
			break
		}
		if len(chain) >= 2 && chain[len(chain)-2] == b {
			chain = chain[:len(chain)-2]
			merges = append(merges, mergeStep{a: a, b: b, height: dist[a*n+b], order: len(merges)})
			merge(a, b, dist, size, live, members, n)
			remaining--
			continue
		}
		chain = append(chain, b)
	}

	// Ward is monotone, so every prefix of the height-sorted merges is a valid
	// tree. Ties keep the order they were produced in, which is deterministic.
	sort.SliceStable(merges, func(i, j int) bool {
		if merges[i].height != merges[j].height {
			return merges[i].height < merges[j].height
		}
		return merges[i].order < merges[j].order
	})

	return cutAt(sp, merges, k)
}

// mergeStep is one join in the dendrogram: which two clusters, at what height,
// and where in the walk it was produced.
type mergeStep struct {
	a, b   int
	height float32
	order  int
}

// cutAt applies joins in the given order until k clusters remain, which is the
// partition the dendrogram has at that height.
func cutAt(sp *embed.Space, merges []mergeStep, k int) *Result {
	n := len(sp.Rows)
	parent := make([]int, n)
	for i := range parent {
		parent[i] = i
	}
	// Iterative with path halving, so no recursion and no deep stacks.
	find := func(x int) int {
		for parent[x] != x {
			parent[x] = parent[parent[x]]
			x = parent[x]
		}
		return x
	}

	groups := n
	for _, m := range merges {
		if groups <= k {
			break
		}
		ra, rb := find(m.a), find(m.b)
		if ra == rb {
			continue
		}
		// The lower index wins so the outcome does not depend on which side of
		// the join the walk arrived from.
		if rb < ra {
			ra, rb = rb, ra
		}
		parent[rb] = ra
		groups--
	}

	byRoot := map[int][]int{}
	for i := range n {
		r := find(i)
		byRoot[r] = append(byRoot[r], i)
	}
	roots := make([]int, 0, len(byRoot))
	for r := range byRoot {
		roots = append(roots, r)
	}
	sort.Ints(roots) // deterministic cluster numbering

	members := make([][]int, len(roots))
	live := make([]bool, len(roots))
	for i, r := range roots {
		members[i], live[i] = byRoot[r], true
	}
	return fromMembers(sp, members, live)
}

// pairwise is the squared Euclidean distance between every pair, which for
// normalized vectors is twice one minus their cosine.
func pairwise(ctx context.Context, sp *embed.Space) []float32 {
	if x, dim, ok := denseRows(sp); ok {
		return pairwiseDense(ctx, x, len(sp.Rows), dim)
	}
	return pairwiseSparse(ctx, sp)
}

// denseRows flattens the space into one row-major float64 matrix, when every
// row holds every dimension.
//
// After the factorization they always do — the whole point of it is to replace
// a sparse term vector with a few hundred latent values — so the sparse dot
// product is walking two identical index arrays and comparing them element by
// element to discover that they match. Handing the same numbers to a matrix
// multiply instead removes that entirely.
func denseRows(sp *embed.Space) ([]float64, int, bool) {
	dim := sp.Dim
	if dim <= 0 || len(sp.Rows) == 0 {
		return nil, 0, false
	}
	for _, r := range sp.Rows {
		if len(r.Idx) != dim || len(r.Val) != dim {
			return nil, 0, false
		}
		for k, idx := range r.Idx {
			if int(idx) != k {
				return nil, 0, false
			}
		}
	}
	x := make([]float64, len(sp.Rows)*dim)
	for i, r := range sp.Rows {
		for k, v := range r.Val {
			x[i*dim+k] = float64(v)
		}
	}
	return x, dim, true
}

// pairwiseDense computes the same matrix as pairwiseSparse through one
// blocked matrix multiply.
//
// The Gram matrix X.Xᵀ holds every dot product, and the distance wanted is
// 2(1-cosine) for L2-normalized rows. It is computed a block of rows at a time
// rather than whole, because the full float64 Gram matrix of a few thousand
// repositories is over a hundred megabytes while the answer, kept in float32,
// is a fraction of that.
func pairwiseDense(ctx context.Context, x []float64, n, dim int) []float32 {
	d := make([]float32, n*n)
	const block = 512
	buf := make([]float64, min(block, n)*n)
	all := blas64.General{Rows: n, Cols: dim, Stride: dim, Data: x}

	for start := 0; start < n; start += block {
		if ctx.Err() != nil {
			return d
		}
		end := min(start+block, n)
		rows := end - start
		blas64.Gemm(blas.NoTrans, blas.Trans, 1,
			blas64.General{Rows: rows, Cols: dim, Stride: dim, Data: x[start*dim : end*dim]},
			all, 0,
			blas64.General{Rows: rows, Cols: n, Stride: n, Data: buf[:rows*n]})

		for i := range rows {
			row := buf[i*n : i*n+n]
			out := d[(start+i)*n : (start+i)*n+n]
			for j, dot := range row {
				v := float32(2 * (1 - dot))
				if v < 0 {
					v = 0
				}
				out[j] = v
			}
			out[start+i] = 0
		}
	}
	return d
}

// pairwiseSparse is the general form, for a space whose rows are not dense —
// the term matrix before it is factored.
func pairwiseSparse(ctx context.Context, sp *embed.Space) []float32 {
	n := len(sp.Rows)
	d := make([]float32, n*n)
	for i := 0; i < n; i++ {
		// One check per row rather than per pair: n checks over an n-squared
		// build, so the cost is negligible and a cancel is still noticed
		// within one row.
		if ctx.Err() != nil {
			return d
		}
		d[i*n+i] = 0
		for j := i + 1; j < n; j++ {
			v := float32(2 * (1 - sp.Rows[i].DotSparse(sp.Rows[j])))
			if v < 0 {
				v = 0
			}
			d[i*n+j] = v
			d[j*n+i] = v
		}
	}
	return d
}

func firstLive(live []bool) int {
	for i, ok := range live {
		if ok {
			return i
		}
	}
	return 0
}

func nearestTo(a int, dist []float32, live []bool, n int) int {
	best, bestD := -1, float32(math.MaxFloat32)
	for j := 0; j < n; j++ {
		if j == a || !live[j] {
			continue
		}
		// Ties are broken by the lower index so that the result does not
		// depend on iteration order.
		if d := dist[a*n+j]; d < bestD {
			best, bestD = j, d
		}
	}
	return best
}

// merge folds b into a and updates a's distances by the Lance-Williams rule
// for Ward linkage.
func merge(a, b int, dist []float32, size []int, live []bool, members [][]int, n int) {
	na, nb := size[a], size[b]
	dab := float64(dist[a*n+b])
	for x := 0; x < n; x++ {
		if x == a || x == b || !live[x] {
			continue
		}
		nx := size[x]
		dax, dbx := float64(dist[a*n+x]), float64(dist[b*n+x])
		v := ((float64(na+nx))*dax + (float64(nb+nx))*dbx - float64(nx)*dab) /
			float64(na+nb+nx)
		dist[a*n+x] = float32(v)
		dist[x*n+a] = float32(v)
	}
	members[a] = append(members[a], members[b]...)
	members[b] = nil
	size[a] = na + nb
	live[b] = false
}

func fromMembers(sp *embed.Space, members [][]int, live []bool) *Result {
	res := &Result{Assign: make([]int, len(sp.Rows))}
	for i := range res.Assign {
		res.Assign[i] = -1
	}
	for i, ok := range live {
		if !ok || len(members[i]) == 0 {
			continue
		}
		c := res.K
		res.K++
		res.Centroids = append(res.Centroids, make([]float64, sp.Dim))
		for _, m := range members[i] {
			res.Assign[m] = c
		}
	}
	recomputeCentroids(sp, res)

	score := 0.0
	for i, c := range res.Assign {
		if c >= 0 {
			score += sp.Rows[i].Dot(res.Centroids[c])
		}
	}
	if len(res.Assign) > 0 {
		res.Score = score / float64(len(res.Assign))
	}
	return res
}

func singletons(sp *embed.Space) *Result {
	res := &Result{Assign: make([]int, len(sp.Rows)), K: len(sp.Rows)}
	res.Centroids = make([][]float64, len(sp.Rows))
	for i := range sp.Rows {
		res.Assign[i] = i
		res.Centroids[i] = make([]float64, sp.Dim)
	}
	recomputeCentroids(sp, res)
	return res
}
