package cluster

import (
	"context"
	"math"
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/mrueg/constellation/internal/embed"
)

// A brand-new account, or one filtered down to almost nothing, reaches the
// clustering with fewer repositories than the requested number of categories.
// Every stage has to survive that rather than dividing by a zero count or
// naming a cluster that was never formed.
func TestClusteringSurvivesTinyCorpora(t *testing.T) {
	for _, n := range []int{0, 1, 2, 3} {
		sp := &embed.Space{Dim: 2, Terms: []string{"a", "b"}}
		for i := range n {
			sp.Rows = append(sp.Rows, embed.Vector{
				Idx: []int32{0, 1},
				Val: []float32{float32(i + 1), 1},
			})
		}
		res := Agglomerate(sp, 8)
		if res == nil {
			t.Fatalf("n=%d: no result", n)
		}
		if len(res.Assign) != n {
			t.Fatalf("n=%d: %d assignments", n, len(res.Assign))
		}
		for i, c := range res.Assign {
			if c >= res.K || c < -1 {
				t.Errorf("n=%d: row %d assigned to cluster %d of %d", n, i, c, res.K)
			}
		}
		if res.K > 0 && len(res.Centroids) != res.K {
			t.Errorf("n=%d: K=%d but %d centroids", n, res.K, len(res.Centroids))
		}
	}
}

// Clustering is the long pure-CPU stage; a cancelled context has to stop it
// rather than run to completion. The result is discarded by the caller, so all
// that matters is that it returns promptly without panicking.
func TestAgglomerateStopsOnCancel(t *testing.T) {
	sp := &embed.Space{Dim: 2, Terms: []string{"a", "b"}}
	for i := range 400 {
		sp.Rows = append(sp.Rows, embed.Vector{
			Idx: []int32{0, 1},
			Val: []float32{float32(i%7 + 1), float32(i%3 + 1)},
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled: the loops must bail on their first check

	res := AgglomerateContext(ctx, sp, 8)
	if res == nil {
		t.Fatal("no result")
	}
	// It bailed early, so it did not merge down to k; that is expected and
	// fine because the caller throws this away. The contract is only that it
	// returns a well-formed, non-panicking Result.
	for i, c := range res.Assign {
		if c < -1 || c >= res.K {
			t.Fatalf("row %d has out-of-range cluster %d (K=%d)", i, c, res.K)
		}
	}
}

// The nearest-neighbour chain reaches the right tree but emits its joins out
// of height order — it descends into one branch and merges deep inside it
// before returning to shallower pairs. Stopping after n-k joins therefore cut
// the tree wherever the walk happened to be, not at the k tallest joins.
//
// Three tight groups placed far apart have an unambiguous 3-cluster answer, so
// any cut that is not the true one is visible.
func TestAgglomerateCutsAtTheRightHeight(t *testing.T) {
	// Unit vectors, because the distance is 2(1-cosine) and assumes them.
	// Three tight groups spread around the circle.
	sp := &embed.Space{Dim: 2, Terms: []string{"x", "y"}}
	for _, centre := range []float64{0, 2.1, 4.2} {
		for i := range 4 {
			a := centre + float64(i)*0.002
			sp.Rows = append(sp.Rows, embed.Vector{
				Idx: []int32{0, 1},
				Val: []float32{float32(math.Cos(a)), float32(math.Sin(a))},
			})
		}
	}

	res := Agglomerate(sp, 3)
	if res.K != 3 {
		t.Fatalf("K = %d, want 3", res.K)
	}
	// Members of a group must share a cluster: that is what cutting at the
	// three tallest joins means.
	for g := range 3 {
		first := res.Assign[g*4]
		for i := 1; i < 4; i++ {
			if got := res.Assign[g*4+i]; got != first {
				t.Errorf("group %d was split across clusters %d and %d", g, first, got)
			}
		}
	}
}

// Cutting the same tree at successive values of k must be nested: going from
// k to k-1 may only join two clusters, never rearrange them. That is the
// property a height-ordered cut has and an emission-ordered one does not.
func TestAgglomerateCutsAreNested(t *testing.T) {
	sp := &embed.Space{Dim: 2, Terms: []string{"x", "y"}}
	for i := range 40 {
		a := float64(i) * 0.157
		sp.Rows = append(sp.Rows, embed.Vector{
			Idx: []int32{0, 1},
			Val: []float32{float32(math.Cos(a)), float32(math.Sin(a))},
		})
	}
	coarse := Agglomerate(sp, 4)
	fine := Agglomerate(sp, 5)

	// Every pair together at k=5 must still be together at k=4.
	for i := range sp.Rows {
		for j := i + 1; j < len(sp.Rows); j++ {
			if fine.Assign[i] == fine.Assign[j] && coarse.Assign[i] != coarse.Assign[j] {
				t.Fatalf("rows %d and %d share a cluster at k=5 but not at k=4; the cuts are not nested", i, j)
			}
		}
	}
}

// wardReference is the textbook algorithm: repeatedly merge the globally
// closest pair. It is O(n^3) and far too slow for real use, which is why the
// nearest-neighbour chain exists — but on a small input it says exactly what
// the answer should be.
func wardReference(sp *embed.Space, k int) []int {
	n := len(sp.Rows)
	d := pairwise(context.Background(), sp)
	size := make([]int, n)
	live := make([]bool, n)
	members := make([][]int, n)
	for i := range size {
		size[i], live[i], members[i] = 1, true, []int{i}
	}
	for groups := n; groups > k; groups-- {
		bi, bj, best := -1, -1, float32(math.MaxFloat32)
		for i := range n {
			if !live[i] {
				continue
			}
			for j := i + 1; j < n; j++ {
				if live[j] && d[i*n+j] < best {
					bi, bj, best = i, j, d[i*n+j]
				}
			}
		}
		merge(bi, bj, d, size, live, members, n)
	}
	assign := make([]int, n)
	next := 0
	for i := range n {
		if !live[i] {
			continue
		}
		for _, m := range members[i] {
			assign[m] = next
		}
		next++
	}
	return assign
}

// sameParts reports whether two assignments describe the same grouping,
// ignoring how the clusters happen to be numbered.
func sameParts(a, b []int) bool {
	m := map[int]int{}
	for i := range a {
		if want, seen := m[a[i]]; seen {
			if want != b[i] {
				return false
			}
		} else {
			m[a[i]] = b[i]
		}
	}
	return true
}

// The chain must produce the same partition as merging the globally closest
// pair each time. It did not: the chain emits joins out of height order, and
// stopping after n-k of them cut the tree wherever the walk had reached.
func TestAgglomerateMatchesTheTrueWardCut(t *testing.T) {
	for _, seed := range []int64{1, 2, 3, 4, 5} {
		sp := &embed.Space{Dim: 3, Terms: []string{"x", "y", "z"}}
		r := rand.New(rand.NewSource(seed))
		for range 60 {
			x, y, z := r.NormFloat64(), r.NormFloat64(), r.NormFloat64()
			norm := math.Sqrt(x*x + y*y + z*z)
			sp.Rows = append(sp.Rows, embed.Vector{
				Idx: []int32{0, 1, 2},
				Val: []float32{float32(x / norm), float32(y / norm), float32(z / norm)},
			})
		}
		for _, k := range []int{3, 5, 8} {
			got := Agglomerate(sp, k).Assign
			want := wardReference(sp, k)
			if !sameParts(got, want) {
				t.Errorf("seed %d k=%d: partition differs from the true Ward cut", seed, k)
			}
		}
	}
}

// Names and descriptions were cut by byte, which splits a multi-byte rune in
// half and posts invalid UTF-8 to GitHub. GitHub's own limits are in
// characters, so byte-cutting also truncated non-ASCII names about twice as
// hard as necessary.
func TestNamesAndDescriptionsStayValidUTF8(t *testing.T) {
	for _, s := range []string{
		strings.Repeat("Ü", 40),
		strings.Repeat("日本語", 20),
		"Kubernetes " + strings.Repeat("Ü", 30),
		strings.Repeat("emoji🚀", 12),
	} {
		if got := truncate(s, MaxListName); !utf8.ValidString(got) {
			t.Errorf("truncate(%q) produced invalid UTF-8: %q", s[:12], got)
		}
		if n := len([]rune(truncate(s, MaxListName))); n > MaxListName {
			t.Errorf("truncate kept %d characters, want at most %d", n, MaxListName)
		}
	}
}

// Describe appends an ellipsis after cutting, so the ellipsis has to be part
// of the budget — otherwise the result overruns the limit it was cut to fit.
func TestDescribeRespectsItsLimit(t *testing.T) {
	long := truncate(strings.Repeat("word ", 200), MaxListDescription-1) + "…"
	if n := len([]rune(long)); n > MaxListDescription {
		t.Errorf("description is %d characters, over the %d limit", n, MaxListDescription)
	}
	if !utf8.ValidString(long) {
		t.Error("description is not valid UTF-8")
	}
}

// The dense path exists only to be faster. It must produce the same matrix as
// the sparse one, or the clustering it feeds changes for reasons that have
// nothing to do with the data.
func TestPairwiseDenseMatchesSparse(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	for _, dims := range []int{1, 2, 17, 64} {
		for _, n := range []int{1, 2, 9, 40} {
			sp := &embed.Space{Dim: dims}
			for range n {
				idx := make([]int32, dims)
				val := make([]float32, dims)
				var norm float64
				for k := range dims {
					v := r.NormFloat64()
					idx[k], val[k] = int32(k), float32(v)
					norm += v * v
				}
				norm = math.Sqrt(norm)
				for k := range dims {
					val[k] = float32(float64(val[k]) / norm)
				}
				sp.Rows = append(sp.Rows, embed.Vector{Idx: idx, Val: val})
			}
			ctx := context.Background()
			want := pairwiseSparse(ctx, sp)
			got := pairwise(ctx, sp)
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("dims=%d n=%d: entry %d is %v via BLAS, %v via the sparse path",
						dims, n, i, got[i], want[i])
				}
			}
		}
	}
}

// A space whose rows are genuinely sparse — the term matrix before it is
// factored — must not take the dense path, which assumes every row carries
// every dimension in order.
func TestPairwiseKeepsTheSparsePathWhenRowsAreSparse(t *testing.T) {
	sp := &embed.Space{Dim: 5, Rows: []embed.Vector{
		{Idx: []int32{0, 3}, Val: []float32{0.6, 0.8}},
		{Idx: []int32{3, 4}, Val: []float32{0.8, 0.6}},
	}}
	if _, _, ok := denseRows(sp); ok {
		t.Fatal("a sparse space was mistaken for a dense one")
	}
	got := pairwise(context.Background(), sp)
	want := pairwiseSparse(context.Background(), sp)
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d differs: %v vs %v", i, got[i], want[i])
		}
	}
}

// Rows that are dense but out of order would give the wrong answer if fed to a
// matrix multiply as-is, so they must be rejected too.
func TestDenseRowsRejectsUnorderedIndices(t *testing.T) {
	sp := &embed.Space{Dim: 3, Rows: []embed.Vector{
		{Idx: []int32{0, 2, 1}, Val: []float32{0.1, 0.2, 0.3}},
	}}
	if _, _, ok := denseRows(sp); ok {
		t.Error("indices out of order were accepted as a dense row")
	}
}

// A centroid is the mean of its members. renumber is where membership changes
// settle — dissolved categories are dropped and their members rehomed, and
// MergeDuplicates folds one category wholly into another through it — so it is
// where a stale centroid gets carried forward. After a merge the stored vector
// was literally the centroid of one of the two halves.
//
// It is not cosmetic: the cohesion measured against these vectors is what
// --min-cohesion tests, what Soften's ratio floor comes from, and what is
// written into the plan as each category's cohesion.
func TestRenumberRebuildsCentroids(t *testing.T) {
	// Two well-separated groups on the unit circle.
	sp := &embed.Space{Dim: 2}
	for _, a := range []float64{0, 0.05, 3.1, 3.15} {
		sp.Rows = append(sp.Rows, embed.Vector{
			Idx: []int32{0, 1},
			Val: []float32{float32(math.Cos(a)), float32(math.Sin(a))},
		})
	}

	// The state a merge leaves behind: every row now belongs to cluster 0,
	// but the stored centroid still describes only the first half.
	half := []float64{math.Cos(0.025), math.Sin(0.025)}
	normalize(half)
	r := &Result{
		Assign:    []int{0, 0, 0, 0},
		Centroids: [][]float64{half},
		K:         1,
	}

	got := renumber(sp, r, map[int]bool{0: true})

	// The mean of all four is near zero in x and small in y; what matters is
	// that it is not the half-centroid it inherited.
	want := make([]float64, sp.Dim)
	for _, row := range sp.Rows {
		for j, ix := range row.Idx {
			want[ix] += float64(row.Val[j])
		}
	}
	normalize(want)
	for k := range want {
		if diff := math.Abs(want[k] - got.Centroids[0][k]); diff > 1e-9 {
			t.Fatalf("centroid was not rebuilt from its members: dimension %d is %g, want %g",
				k, got.Centroids[0][k], want[k])
		}
	}
}
