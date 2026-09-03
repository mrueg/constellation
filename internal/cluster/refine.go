package cluster

import (
	"math"
	"sort"

	"github.com/mrueg/constellation/internal/embed"
)

// outlierRelativeGuard is the second half of the outlier test: however tight a
// category is, a repository at more than this fraction of the typical fit is
// kept.
const outlierRelativeGuard = 0.6

// RefineOptions shape the raw k-means output into something worth turning into
// star lists.
type RefineOptions struct {
	// MinSize dissolves clusters with fewer members than this, redistributing
	// them to their next-nearest cluster. A two-repo list is not a category.
	MinSize int
	// MaxLists keeps only the N largest clusters; the rest are dissolved.
	MaxLists int
	// SplitParts is the most pieces an incoherent category may be divided into
	// before it is given up on. Zero dissolves it outright.
	//
	// A residual cluster is not always uniformly bad. It is often a real
	// category with a tail of unrelated repositories swept in, so dissolving it
	// whole throws away the good part along with the rest — one such cluster
	// held a genuine terminal-tools group alongside a calculator and a code
	// review checklist. Dividing it first keeps the part that holds together.
	SplitParts int
	// SplitMinSize is the smallest category worth trying to divide.
	SplitMinSize int
	// SplitMaxSize divides a category larger than this even when it is
	// coherent enough to keep.
	//
	// A large category is not necessarily a bad one, but it is usually several
	// good ones the tree never separated: a real account produced a single
	// Kubernetes list of 366 repositories holding core tooling, operators,
	// networking and cluster lifecycle together. Nothing about its coherence
	// says so, which is why size has to be asked about separately. Zero leaves
	// large categories alone.
	SplitMaxSize int
	// RescueNeighbours is how many nearest neighbours an uncategorized
	// repository consults before joining a category. Zero disables it.
	//
	// It asks a different question from every other test here. The others ask
	// whether a repository resembles a category's centre; this asks whether the
	// repositories most like it agree on where they belong. A repository can
	// fail the first and pass the second — a niche tool sitting at the edge of
	// a broad category is far from its centre but surrounded by its members.
	RescueNeighbours int
	// RescueAgreement is the share of those neighbours that must name the same
	// category before it is joined.
	RescueAgreement float64
	// MinCohesion dissolves a category whose members do not, on average,
	// resemble it enough to be a category at all.
	//
	// This is the one test the per-member outlier check cannot make. That check
	// asks whether a repository fits worse than its category's typical member,
	// which is calibrated against the category itself — so a category that is
	// uniformly incoherent has a low bar and rejects nobody. A bad cluster
	// defends its own members.
	//
	// It matters because k-means has no way to say "this belongs nowhere".
	// Every repository must join some centroid, so the leftovers collect into
	// two or three enormous residual clusters which then get named after
	// whatever small coherent group happens to be inside them. On a real
	// account those held a fifth of all stars under names like "ESP32 Arduino"
	// while containing neither.
	MinCohesion float64
	// MinSimilarity is an absolute floor on how well a repository must fit its
	// category. It only catches the degenerate cases; what counts as a good
	// cosine depends on the embedding, so it is deliberately permissive.
	MinSimilarity float64
	// OutlierSigmas is the useful test: a repository is left uncategorized when
	// its fit falls this many robust deviations below its category's typical
	// fit. Star lists always contain a long tail of one-off repositories that
	// belong to no category, and forcing them into the nearest one is what
	// makes automatic categorization feel wrong. A fixed threshold cannot find
	// them — a pair of misfits that resemble each other drag their shared
	// centroid towards themselves and so clear any absolute bar — while a
	// deviation measured against the category's own spread adapts both to that
	// and to the cosine scale the embedding produces.
	// 0 disables the test.
	OutlierSigmas float64
}

// Refine mutates r in place and returns it, renumbering clusters so that the
// surviving ones are contiguous and ordered by descending size.
func Refine(sp *embed.Space, r *Result, opt RefineOptions) *Result {
	if r == nil || r.K == 0 {
		return r
	}
	// Repositories rejected as outliers must stay rejected: they are exactly
	// the ones the redistribution step below would otherwise happily attach to
	// their second-best category.
	rejected := make([]bool, len(sp.Rows))
	// Two passes. The first centroid is pulled towards the misfits it
	// contains, so dropping the obvious ones and recomputing gives the second
	// pass a centroid that describes the category rather than its noise.
	for pass := 0; pass < 2; pass++ {
		if !dropOutliers(sp, r, opt, rejected) {
			break
		}
		recomputeCentroids(sp, r)
	}

	cohesion := cohesions(sp, r)
	if opt.SplitParts >= 2 && opt.MinCohesion > 0 {
		r = splitWeak(sp, r, opt, cohesion)
	}

	if opt.RescueNeighbours > 0 {
		rescueByNeighbours(sp, r, opt)
		// Rescued repositories join a category without passing through
		// renumber, so their centroids have to be brought in line here.
		recomputeCentroids(sp, r)
	}

	// Dissolving and rehoming are done together, repeatedly, because they
	// interact: every repository moved into a category pulls its average down
	// a little, so a category that passed the coherence test before the moves
	// may fail after them. Each pass drops whatever no longer holds together
	// and offers its members to what remains, until nothing more falls.
	for pass := 0; pass < maxRefinePasses; pass++ {
		cohesion = cohesions(sp, r)
		keep := survivors(r, opt, cohesion)
		rehome(sp, r, keep, cohesion, opt, rejected)
		r = renumber(sp, r, keep)
		if opt.MinCohesion <= 0 || settled(cohesions(sp, r), opt.MinCohesion) {
			break
		}
	}
	return r
}

// maxRefinePasses bounds the dissolve-and-rehome loop. Each pass can only
// remove categories, so it terminates on its own; the bound is there so that a
// pathological corpus cannot spend the afternoon on it.
const maxRefinePasses = 5

func settled(cohesion []float64, floor float64) bool {
	for _, c := range cohesion {
		if c < floor {
			return false
		}
	}
	return true
}

// rehome offers every repository not in a surviving category a place in one,
// which it takes only if it fits well enough to belong there.
func rehome(sp *embed.Space, r *Result, keep map[int]bool, cohesion []float64, opt RefineOptions, rejected []bool) {
	for i, row := range sp.Rows {
		c := r.Assign[i]
		if (c >= 0 && keep[c]) || rejected[i] {
			continue
		}
		bestC, bestS := -1, opt.MinSimilarity
		// In cluster order rather than map order: two categories can be
		// equally close and the winner must not depend on how the map is
		// walked.
		for kc := 0; kc < r.K; kc++ {
			if !keep[kc] {
				continue
			}
			// A repository freed from a dissolved category has to earn its new
			// one. Re-attaching it wherever it is least bad would move the
			// misfiling rather than end it.
			floor := opt.MinSimilarity
			if f := rehomeGuard * cohesion[kc]; f > floor {
				floor = f
			}
			if s := row.Dot(r.Centroids[kc]); s > bestS && s >= floor {
				bestC, bestS = kc, s
			}
		}
		r.Assign[i] = bestC
	}
}

// rehomeGuard is how well a repository from a dissolved category must fit a
// surviving one, as a fraction of that category's typical fit, before it is
// moved there rather than left uncategorized.
const rehomeGuard = 0.6

// cohesions is each cluster's mean similarity to its own centroid.
func cohesions(sp *embed.Space, r *Result) []float64 {
	sums := make([]float64, r.K)
	counts := make([]int, r.K)
	for i, row := range sp.Rows {
		c := r.Assign[i]
		if c < 0 {
			continue
		}
		sums[c] += row.Dot(r.Centroids[c])
		counts[c]++
	}
	out := make([]float64, r.K)
	for c := range out {
		if counts[c] > 0 {
			out[c] = sums[c] / float64(counts[c])
		}
	}
	return out
}

// survivors picks the clusters worth keeping: coherent enough to be a
// category, big enough to be one, and within the caller's cap on how many
// lists to create.
func survivors(r *Result, opt RefineOptions, cohesion []float64) map[int]bool {
	sizes := make([]int, r.K)
	for _, c := range r.Assign {
		if c >= 0 {
			sizes[c]++
		}
	}
	order := make([]int, r.K)
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return sizes[order[a]] > sizes[order[b]] })

	keep := map[int]bool{}
	for _, c := range order {
		if sizes[c] < opt.MinSize {
			continue
		}
		if opt.MinCohesion > 0 && cohesion[c] < opt.MinCohesion {
			continue
		}
		if opt.MaxLists > 0 && len(keep) >= opt.MaxLists {
			break
		}
		keep[c] = true
	}
	return keep
}

func renumber(sp *embed.Space, r *Result, keep map[int]bool) *Result {
	sizes := make([]int, r.K)
	for _, c := range r.Assign {
		if c >= 0 {
			sizes[c]++
		}
	}
	old := make([]int, 0, len(keep))
	for c := range keep {
		if sizes[c] > 0 {
			old = append(old, c)
		}
	}
	sort.SliceStable(old, func(a, b int) bool {
		if sizes[old[a]] != sizes[old[b]] {
			return sizes[old[a]] > sizes[old[b]]
		}
		return old[a] < old[b]
	})

	remap := make(map[int]int, len(old))
	centroids := make([][]float64, len(old))
	for newIdx, c := range old {
		remap[c] = newIdx
		centroids[newIdx] = r.Centroids[c]
	}
	for i := range r.Assign {
		if c, ok := remap[r.Assign[i]]; ok {
			r.Assign[i] = c
		} else {
			r.Assign[i] = -1
		}
		if i < len(r.Also) {
			r.Also[i] = remapAlso(r.Also[i], remap, r.Assign[i])
		}
	}
	r.Centroids = centroids
	r.K = len(centroids)
	// A centroid is the mean of its members, and this function has just
	// changed who the members are — dissolving categories and rehoming what
	// was in them, or, when called from MergeDuplicates, folding one category
	// wholly into another. Carrying the old vectors forward left a category
	// described by a vector belonging to a set that no longer existed; after a
	// merge, literally the centroid of one of its two halves.
	//
	// It matters because the cohesion measured against these vectors is what
	// --min-cohesion tests, what Soften's ratio floor is taken from, and what
	// is printed and written into the plan as each category's cohesion.
	recomputeCentroids(sp, r)
	r.Score = meanFit(sp, r)
	return r
}

// meanFit is the average similarity of a repository to the category it was
// placed in.
func meanFit(sp *embed.Space, r *Result) float64 {
	var score float64
	var assigned int
	for i, row := range sp.Rows {
		c := r.Assign[i]
		if c < 0 || c >= len(r.Centroids) {
			continue
		}
		score += row.Dot(r.Centroids[c])
		assigned++
	}
	if assigned == 0 {
		return math.NaN()
	}
	return score / float64(assigned)
}

// recomputeCentroids re-derives each centroid from the repositories still
// assigned to it, leaving an emptied cluster's centroid untouched so that
// nothing else is silently redirected into it.
func recomputeCentroids(sp *embed.Space, r *Result) {
	sums := make([][]float64, r.K)
	counts := make([]int, r.K)
	for c := range sums {
		sums[c] = make([]float64, sp.Dim)
	}
	for i, row := range sp.Rows {
		c := r.Assign[i]
		if c < 0 {
			continue
		}
		counts[c]++
		for j, ix := range row.Idx {
			sums[c][ix] += float64(row.Val[j])
		}
	}
	for c := range sums {
		if counts[c] == 0 {
			continue
		}
		normalize(sums[c])
		r.Centroids[c] = sums[c]
	}
}

// dropOutliers unassigns repositories that fit their category far worse than
// its typical member, recording them in rejected and reporting whether
// anything changed.
//
// The bar comes from the median and the median absolute deviation rather than
// the mean and standard deviation, because both of those are moved by the very
// points being looked for.
func dropOutliers(sp *embed.Space, r *Result, opt RefineOptions, rejected []bool) bool {
	if opt.MinSimilarity <= 0 && opt.OutlierSigmas <= 0 {
		return false
	}
	sims := make([]float64, len(sp.Rows))
	perCluster := make([][]float64, r.K)
	for i, row := range sp.Rows {
		c := r.Assign[i]
		if c < 0 {
			continue
		}
		sims[i] = row.Dot(r.Centroids[c])
		perCluster[c] = append(perCluster[c], sims[i])
	}

	floors := make([]float64, r.K)
	for c := range floors {
		floors[c] = opt.MinSimilarity
		if opt.OutlierSigmas <= 0 || len(perCluster[c]) < 4 {
			continue
		}
		med := median(perCluster[c])
		devs := make([]float64, len(perCluster[c]))
		for i, v := range perCluster[c] {
			devs[i] = math.Abs(v - med)
		}
		mad := median(devs)
		// A cluster whose members all fit identically well has no spread to
		// measure against, and subtracting nothing from the median would
		// reject half of it.
		if mad < 1e-6 {
			continue
		}
		// A repository has to fail both tests to be dropped: statistically
		// anomalous for this category, and substantially worse than a typical
		// member of it. Requiring only the first throws away good members of a
		// category whose core happens to be very tight, where three deviations
		// is still a hair's breadth.
		f := math.Min(med-opt.OutlierSigmas*mad, outlierRelativeGuard*med)
		if f > floors[c] {
			floors[c] = f
		}
	}

	changed := false
	for i := range sp.Rows {
		if c := r.Assign[i]; c >= 0 && sims[i] < floors[c] {
			r.Assign[i] = -1
			// A repository that fits its best category this poorly has no
			// business in its second-best either.
			if i < len(r.Also) {
				r.Also[i] = nil
			}
			rejected[i] = true
			changed = true
		}
	}
	return changed
}

func median(xs []float64) float64 {
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	mid := len(s) / 2
	if len(s)%2 == 1 {
		return s[mid]
	}
	return (s[mid-1] + s[mid]) / 2
}

// remapAlso renumbers secondary memberships onto the surviving clusters,
// dropping any that were dissolved and any that duplicate the primary.
func remapAlso(also []int, remap map[int]int, primary int) []int {
	var out []int
	for _, c := range also {
		n, ok := remap[c]
		if !ok || n == primary {
			continue
		}
		out = append(out, n)
	}
	return out
}

// splitWeak divides each incoherent category into parts and keeps the parts
// that hold together, leaving the rest to be dissolved as before.
//
// Only the categories present on entry are considered, so a piece produced
// here is never itself split again: the point is to rescue a coherent group
// from a residual bucket, not to keep subdividing until everything is a
// singleton.
func splitWeak(sp *embed.Space, r *Result, opt RefineOptions, cohesion []float64) *Result {
	original := r.K
	sizes := make([]int, original)
	for _, c := range r.Assign {
		if c >= 0 {
			sizes[c]++
		}
	}
	for c := 0; c < original; c++ {
		incoherent := cohesion[c] < opt.MinCohesion
		oversized := opt.SplitMaxSize > 0 && sizes[c] > opt.SplitMaxSize
		if !incoherent && !oversized {
			continue
		}
		if sizes[c] < opt.SplitMinSize {
			continue
		}
		members := r.Members(c)
		if len(members) < opt.SplitMinSize {
			continue
		}
		best, bestRescued := (*Result)(nil), 0
		for parts := 2; parts <= opt.SplitParts && parts <= len(members); parts++ {
			// Ward linkage again rather than k-means: the whole point of a
			// deterministic clustering is lost if the step that rescues its
			// weak categories reintroduces a random start.
			sub := subspace(sp, members)
			cand := Agglomerate(sub, parts)
			if n := rescued(sub, cand, opt); n > bestRescued {
				best, bestRescued = cand, n
			}
		}
		// A category split for size alone is kept whole unless the division
		// accounts for nearly all of it: losing half a good list to rescue the
		// other half is a worse outcome than leaving it large.
		if best == nil || bestRescued == 0 {
			continue
		}
		if !incoherent && float64(bestRescued) < 0.9*float64(len(members)) {
			continue
		}
		applySplit(sp, r, members, best, opt)
	}
	return r
}

// rescued counts the repositories a candidate split would place in a part that
// is coherent and large enough to stand on its own.
func rescued(sub *embed.Space, cand *Result, opt RefineOptions) int {
	sizes := make([]int, cand.K)
	for _, c := range cand.Assign {
		if c >= 0 {
			sizes[c]++
		}
	}
	co := cohesions(sub, cand)
	total := 0
	for c := 0; c < cand.K; c++ {
		if sizes[c] >= opt.MinSize && co[c] >= opt.MinCohesion {
			total += sizes[c]
		}
	}
	return total
}

// applySplit rewrites the parent category into its surviving parts. Members of
// a part that did not hold together are released, and the ordinary
// redistribution decides whether they fit anywhere else.
func applySplit(sp *embed.Space, r *Result, members []int, cand *Result, opt RefineOptions) {
	sub := subspace(sp, members)
	sizes := make([]int, cand.K)
	for _, c := range cand.Assign {
		if c >= 0 {
			sizes[c]++
		}
	}
	co := cohesions(sub, cand)

	newIndex := make([]int, cand.K)
	for c := range newIndex {
		newIndex[c] = -1
	}
	for c := 0; c < cand.K; c++ {
		if sizes[c] < opt.MinSize || co[c] < opt.MinCohesion {
			continue
		}
		newIndex[c] = r.K
		r.Centroids = append(r.Centroids, cand.Centroids[c])
		r.K++
	}
	for i, m := range members {
		part := cand.Assign[i]
		if part < 0 {
			r.Assign[m] = -1
			continue
		}
		r.Assign[m] = newIndex[part]
		if m < len(r.Also) {
			r.Also[m] = nil
		}
	}
}

// subspace is the rows of a single category, kept in the full dimension so
// that the centroids it produces need no translation.
func subspace(sp *embed.Space, members []int) *embed.Space {
	rows := make([]embed.Vector, len(members))
	for i, m := range members {
		rows[i] = sp.Rows[m]
	}
	return &embed.Space{Dim: sp.Dim, Rows: rows, Backend: sp.Backend}
}

// rescueByNeighbours gives every uncategorized repository the category its
// nearest categorized neighbours agree on, when they agree strongly enough.
//
// It runs before the coherence loop rather than after, so that anything it
// drags below the floor is dissolved by the same machinery as everything else.
func rescueByNeighbours(sp *embed.Space, r *Result, opt RefineOptions) {
	type neighbour struct {
		sim float64
		cat int
	}
	k := opt.RescueNeighbours
	agreement := opt.RescueAgreement
	if agreement <= 0 {
		agreement = 0.6
	}

	decided := make([]int, len(sp.Rows))
	for i := range decided {
		decided[i] = -1
	}
	for i, row := range sp.Rows {
		if r.Assign[i] >= 0 {
			continue
		}
		best := make([]neighbour, 0, k+1)
		for j, other := range sp.Rows {
			c := r.Assign[j]
			if c < 0 || i == j {
				continue
			}
			sim := row.DotSparse(other)
			if len(best) == k && sim <= best[len(best)-1].sim {
				continue
			}
			best = append(best, neighbour{sim, c})
			for a := len(best) - 1; a > 0 && best[a].sim > best[a-1].sim; a-- {
				best[a], best[a-1] = best[a-1], best[a]
			}
			if len(best) > k {
				best = best[:k]
			}
		}
		if len(best) == 0 {
			continue
		}
		votes := map[int]int{}
		for _, n := range best {
			votes[n.cat]++
		}
		// Strongest vote wins, ties broken by the lower category index. Taking
		// whichever category the map happened to yield first made the result
		// depend on Go's randomized map order whenever two cleared the bar —
		// unreachable at the default agreement, but not below it.
		bestCat, bestVotes := -1, 0
		for cat, n := range votes {
			if n > bestVotes || (n == bestVotes && cat < bestCat) {
				bestCat, bestVotes = cat, n
			}
		}
		if bestCat >= 0 && float64(bestVotes) >= agreement*float64(len(best)) {
			decided[i] = bestCat
		}
	}
	// Applied only after every decision is made, so that one rescue cannot
	// become evidence for the next.
	for i, c := range decided {
		if c >= 0 {
			r.Assign[i] = c
		}
	}
}
