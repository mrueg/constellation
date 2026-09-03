package cluster

import "github.com/mrueg/constellation/internal/embed"

// Soften adds secondary list memberships, so that a repository which belongs
// in more than one category is filed in more than one.
//
// Star lists are many-to-many and a Rust command line tool genuinely belongs
// under both headings, but every clustering algorithm here returns a
// partition. This is the step that recovers what the partition threw away, and
// it is deliberately separate from the algorithms: it applies equally to all
// of them, and it runs after refinement so that it can only ever add a
// repository to a list that survived.
//
// Membership is judged by how a repository's similarity to another centroid
// compares with the similarity to its own, rather than by an absolute number
// or by a mixture's posteriors. A ratio means the same thing whatever the
// backend and however many dimensions it produces; posteriors do not, since
// they saturate to zero and one as dimensions grow, and an absolute cosine
// threshold is differently strict for a tight category than a broad one.
//
// ratio is how close a further category must come, as a fraction of the best
// fit; maxLists caps the total a single repository may join. A maxLists of one
// leaves the partition alone.
func Soften(sp *embed.Space, r *Result, ratio float64, maxLists int) *Result {
	if r == nil || r.K == 0 || maxLists <= 1 || ratio <= 0 {
		return r
	}
	if len(r.Also) != len(r.Assign) {
		r.Also = make([][]int, len(r.Assign))
	}
	for i, row := range sp.Rows {
		primary := r.Assign[i]
		if primary < 0 {
			r.Also[i] = nil
			continue
		}
		floor := ratio * row.Dot(r.Centroids[primary])
		if floor <= 0 {
			r.Also[i] = nil
			continue
		}
		type cand struct {
			c   int
			sim float64
		}
		var also []cand
		for c := range r.Centroids {
			if c == primary {
				continue
			}
			if sim := row.Dot(r.Centroids[c]); sim >= floor {
				also = append(also, cand{c, sim})
			}
		}
		// Best fit first, so that the cap keeps the memberships worth keeping.
		for a := range also {
			for b := a + 1; b < len(also); b++ {
				if also[b].sim > also[a].sim {
					also[a], also[b] = also[b], also[a]
				}
			}
		}
		if len(also) > maxLists-1 {
			also = also[:maxLists-1]
		}
		r.Also[i] = nil
		for _, c := range also {
			r.Also[i] = append(r.Also[i], c.c)
		}
	}
	return r
}
