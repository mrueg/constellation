package cluster

import (
	"github.com/mrueg/constellation/internal/embed"
	"github.com/mrueg/constellation/internal/textproc"
)

// DropUnnamed dissolves categories that are not really about the thing they
// are named after.
//
// A junk drawer is not detectable by coherence. A real account produced a
// category of 105 repositories called "Retries & Mob" — retry libraries, mob
// programming, KDE and Debian packaging together — at cohesion 0.49, squarely
// mid-table and comfortably above any floor worth setting. Vectors can be
// generically alike without being about anything, and the average distance to
// their own centre does not notice.
//
// What does notice is asking how many members carry the term the category is
// named after. On that account every genuine category scored at least 25% and
// most above 68%; the junk drawer scored 2%. A name that describes two
// repositories out of a hundred is not a name, and a category that cannot be
// named is not a category.
//
// Members are released rather than moved: they were swept together precisely
// because they fitted nothing, so reporting them as uncategorized is the
// honest outcome.
func DropUnnamed(docs []textproc.Doc, sp *embed.Space, r *Result, minCoverage float64) *Result {
	if minCoverage <= 0 || r == nil || r.K == 0 {
		return r
	}
	labels := Names(docs, r.Assign, r.K, 6)
	keep := map[int]bool{}
	for c := 0; c < r.K; c++ {
		if coverage(docs, r.Members(c), labels[c].Lead) >= minCoverage {
			keep[c] = true
		}
	}
	if len(keep) == r.K {
		return r
	}
	for i, c := range r.Assign {
		if c >= 0 && !keep[c] {
			r.Assign[i] = -1
			if i < len(r.Also) {
				r.Also[i] = nil
			}
		}
	}
	return renumber(sp, r, keep)
}

// coverage is the share of a category's members carrying the given term.
func coverage(docs []textproc.Doc, members []int, term string) float64 {
	if term == "" || len(members) == 0 {
		return 0
	}
	// A category can be named after a phrase, and phrases are kept apart from
	// single terms. Consulting only the latter scored every phrase-named
	// category at zero and would have dissolved all of them.
	hits := 0
	for _, m := range members {
		if docs[m].Terms[term] > 0 || docs[m].Bigrams[term] > 0 {
			hits++
		}
	}
	return float64(hits) / float64(len(members))
}
