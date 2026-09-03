package embed

import (
	"context"
	"math"
	"sort"
)

// TFIDF builds the weighted term matrix: sublinear term frequency times
// smoothed inverse document frequency, restricted to a vocabulary of the most
// discriminative terms.
//
// It is not offered as a backend of its own. Grouping repositories directly on
// these vectors only finds those that share wording, which is the limitation
// LSA exists to remove; this is the matrix LSA factors.
type TFIDF struct {
	// MinDF drops terms that appear in fewer than this many repositories; they
	// carry no grouping signal and only add noise.
	MinDF int
	// MaxDFRatio drops terms that appear in more than this fraction of the
	// corpus, which is how "kubernetes" stops dominating a Kubernetes-heavy
	// star list.
	MaxDFRatio float64
	// MaxVocab caps the vocabulary, keeping the highest-IDF-weighted terms.
	MaxVocab int

	// BM25 switches term weighting from classic TF-IDF to Okapi BM25: term
	// frequency saturates, so the twentieth occurrence of a word adds almost
	// nothing over the fifth, and document length is corrected for by a pivot
	// rather than merely rescaled. It is the textbook choice for a corpus of
	// uneven document lengths, which is what READMEs create here.
	BM25 bool
	// K1 sets how quickly term frequency saturates; B how much length counts.
	K1 float64
	B  float64
}

// idfOf is how much a term's rarity counts. BM25's form approaches zero for a
// term in nearly every document; the classic form keeps a floor of one.
func (t *TFIDF) idfOf(n, df int) float64 {
	if t.BM25 {
		return math.Log(1 + (float64(n)-float64(df)+0.5)/(float64(df)+0.5))
	}
	return math.Log(float64(n+1)/float64(df+1)) + 1
}

// weightOf scores one term in one document.
func (t *TFIDF) weightOf(tf, idf, length, avgLen float64) float64 {
	if !t.BM25 {
		// Sublinear term frequency, 1 + log(tf), assumes tf is a count of at
		// least one. Here it is an accumulated field weight, and those are
		// fractional: a term seen only in a README arrives as 0.15, and
		// 1 + log(0.15) is -0.9. That made 73% of the matrix negative, so two
		// repositories sharing a rare term were pushed apart rather than
		// together, and cosine similarity left [0,1] entirely — which every
		// threshold downstream assumes.
		//
		// Below 1 the weight is simply proportional instead. That is
		// continuous at tf = 1, monotone over the whole domain, never
		// negative, and leaves every tf >= 1 scoring exactly as before, so
		// the tuning measured against descriptions and topics still holds.
		if tf < 1 {
			return tf * idf
		}
		return (1 + math.Log(tf)) * idf
	}
	k1, b := t.K1, t.B
	if k1 <= 0 {
		k1 = 1.2
	}
	if b < 0 || b > 1 {
		b = 0.75
	}
	return idf * tf * (k1 + 1) / (tf + k1*(1-b+b*length/avgLen))
}

func (t *TFIDF) Name() string { return "tfidf" }

func (t *TFIDF) Embed(_ context.Context, docs []Doc) (*Space, error) {
	n := len(docs)
	df := map[string]int{}
	for _, d := range docs {
		for term := range d.Terms {
			df[term]++
		}
	}

	maxDF := int(math.Ceil(t.MaxDFRatio * float64(n)))
	if maxDF < t.MinDF {
		maxDF = n
	}
	type cand struct {
		term string
		idf  float64
		// weight ranks the term for the vocabulary cap: how much total
		// evidence it carries across the corpus, not how rare it is.
		weight float64
	}
	var words []cand
	for _, term := range sortedKeys(mapToFloat(df)) {
		c := df[term]
		if c < t.MinDF || c > maxDF {
			continue
		}
		idf := t.idfOf(n, c)
		words = append(words, cand{term: term, idf: idf, weight: float64(c) * idf})
	}
	// When the cap bites, keep the terms carrying the most evidence — document
	// frequency times inverse document frequency — rather than the rarest.
	//
	// Ranking by inverse document frequency alone means ranking by rarity,
	// since one is a decreasing function of the other. That is harmless while
	// the band already fits under the cap, and ruinous the moment it does not:
	// with --min-df 1 this corpus offers 14,209 terms seen in exactly one
	// repository against a cap of 12,000, so the entire vocabulary became
	// words that by construction group nothing, and every mid-frequency term —
	// the ones that make categories cohere — was discarded. Ties break
	// alphabetically so the pipeline stays deterministic.
	sort.SliceStable(words, func(i, j int) bool {
		if words[i].weight != words[j].weight {
			return words[i].weight > words[j].weight
		}
		return words[i].term < words[j].term
	})
	if t.MaxVocab > 0 && len(words) > t.MaxVocab {
		words = words[:t.MaxVocab]
	}
	cands := words

	vocab := make(map[string]int32, len(cands))
	terms := make([]string, len(cands))
	idf := make([]float64, len(cands))
	sorted := append([]cand(nil), cands...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].term < sorted[j].term })
	for i, c := range sorted {
		vocab[c.term] = int32(i)
		terms[i] = c.term
		idf[i] = c.idf
	}

	// BM25 discounts a document for being longer than average, so lengths have
	// to be known before any weight can be computed.
	lengths := make([]float64, n)
	avgLen := 1.0
	if t.BM25 {
		var total float64
		for i, d := range docs {
			for term, w := range d.Terms {
				if _, ok := vocab[term]; ok {
					lengths[i] += w
				}
			}
			total += lengths[i]
		}
		if n > 0 && total > 0 {
			avgLen = total / float64(n)
		}
	}

	rows := make([]Vector, n)
	for i, d := range docs {
		var idxs []int32
		for _, term := range sortedKeys(d.Terms) {
			if ix, ok := vocab[term]; ok {
				idxs = append(idxs, ix)
			}
		}
		sort.Slice(idxs, func(a, b int) bool { return idxs[a] < idxs[b] })
		vals := make([]float32, len(idxs))
		for k, ix := range idxs {
			raw := d.Terms[terms[ix]]
			if raw <= 0 {
				continue
			}
			vals[k] = float32(t.weightOf(raw, idf[ix], lengths[i], avgLen))
		}
		rows[i] = normalizeInto(idxs, vals)
	}
	return &Space{Dim: len(terms), Rows: rows, Terms: terms, Backend: t.Name()}, nil
}

func mapToFloat(m map[string]int) map[string]float64 {
	out := make(map[string]float64, len(m))
	for k, v := range m {
		out[k] = float64(v)
	}
	return out
}
