// Package embed turns tokenized repositories into L2-normalized vectors that
// the clustering stage can compare with a plain dot product.
package embed

import (
	"context"
	"math"
	"sort"

	"github.com/mrueg/constellation/internal/textproc"
)

// Vector is a sparse L2-normalized vector. Idx is sorted ascending and the
// same length as Val. TF-IDF produces genuinely sparse rows; dense backends
// simply fill every index.
type Vector struct {
	Idx []int32
	Val []float32
}

// Dot computes v·centroid where centroid is dense and len(centroid) >= Dim.
func (v Vector) Dot(centroid []float64) float64 {
	var s float64
	for i, ix := range v.Idx {
		s += float64(v.Val[i]) * centroid[ix]
	}
	return s
}

// DotSparse computes the cosine similarity between two normalized sparse
// vectors by merging their sorted index lists.
func (v Vector) DotSparse(o Vector) float64 {
	var s float64
	i, j := 0, 0
	for i < len(v.Idx) && j < len(o.Idx) {
		switch {
		case v.Idx[i] < o.Idx[j]:
			i++
		case v.Idx[i] > o.Idx[j]:
			j++
		default:
			s += float64(v.Val[i]) * float64(o.Val[j])
			i++
			j++
		}
	}
	return s
}

// Space is a corpus embedded into a shared vector space. Terms maps a
// dimension back to the term that produced it, when the backend has one; it is
// nil for opaque neural embeddings.
type Space struct {
	Dim     int
	Rows    []Vector
	Terms   []string
	Backend string
}

// Embedder builds a Space from the corpus. Docs are index-aligned with the
// repositories being categorized.
type Embedder interface {
	Name() string
	Embed(ctx context.Context, docs []Doc) (*Space, error)
}

// Doc is a weighted bag of terms, produced by the textproc package.
type Doc = textproc.Doc

// Normalize builds an L2-normalized sparse vector. It takes ownership of both
// slices.
func Normalize(idx []int32, val []float32) Vector { return normalizeInto(idx, val) }

func normalizeInto(idx []int32, val []float32) Vector {
	var norm float64
	for _, v := range val {
		norm += float64(v) * float64(v)
	}
	if norm == 0 {
		return Vector{Idx: idx, Val: val}
	}
	inv := float32(1 / math.Sqrt(norm))
	for i := range val {
		val[i] *= inv
	}
	return Vector{Idx: idx, Val: val}
}

func sortedKeys(m map[string]float64) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
