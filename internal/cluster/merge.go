package cluster

import (
	"strings"

	"github.com/mrueg/constellation/internal/embed"
	"github.com/mrueg/constellation/internal/textproc"
)

// MergeDuplicates folds together categories that describe the same thing.
//
// Cutting a dendrogram at a fixed height sometimes separates one category into
// two. A real account produced "Prometheus Exporter" and "Exporter & Metrics"
// side by side, both at cohesion 0.75, sharing most of their terms — two lists
// nobody would have made on purpose.
//
// The test is the overlap between their describing terms, not the distance
// between their centroids. In a latent space every centroid points broadly the
// same way, so unrelated categories already look similar there and a test
// built on it collapses everything into a handful of lists; whereas the terms
// that distinguish a category from the others are, by construction, what makes
// it a different category. On that account the duplicated pair overlapped by
// 0.33 and the next closest pair by 0.20, which is a gap wide enough to sit a
// threshold in.
func MergeDuplicates(docs []textproc.Doc, sp *embed.Space, r *Result, minOverlap float64) *Result {
	if minOverlap <= 0 || r == nil || r.K < 2 {
		return r
	}
	for r.K > 1 {
		// The same number of terms the plan itself displays, so the threshold
		// means what it appears to mean when comparing two categories by eye.
		labels := Names(docs, r.Assign, r.K, mergeTerms)
		bestA, bestB, best := -1, -1, minOverlap
		for a := 0; a < r.K; a++ {
			for b := a + 1; b < r.K; b++ {
				if o := overlap(labels[a].TopTerms, labels[b].TopTerms); o >= best {
					bestA, bestB, best = a, b, o
				}
			}
		}
		if bestA < 0 {
			return r
		}
		for i, c := range r.Assign {
			if c == bestB {
				r.Assign[i] = bestA
			}
		}
		keep := map[int]bool{}
		for c := 0; c < r.K; c++ {
			if c != bestB {
				keep[c] = true
			}
		}
		r = renumber(sp, r, keep)
	}
	return r
}

// mergeTerms is how many describing terms the comparison uses. It matches what
// a plan prints: with more terms the lists diverge in their tails and every
// overlap shrinks, which would make the threshold mean something different
// from what a reader comparing two categories would judge.
const mergeTerms = 6

// overlap is the Jaccard similarity of two term lists, compared case-blind
// because the terms are display forms.
func overlap(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	set := make(map[string]bool, len(a))
	for _, t := range a {
		set[strings.ToLower(t)] = true
	}
	shared := 0
	union := len(set)
	for _, t := range b {
		if k := strings.ToLower(t); set[k] {
			shared++
		} else {
			union++
		}
	}
	return float64(shared) / float64(union)
}
