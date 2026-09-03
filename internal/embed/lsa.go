package embed

import (
	"context"
	"fmt"
	"math"
	"math/rand"

	"gonum.org/v1/gonum/mat"
)

// LSA is latent semantic analysis: a truncated singular value decomposition of
// the TF-IDF matrix.
//
// It exists to fix what TF-IDF alone cannot. Term matching only groups
// repositories that share vocabulary, so a "container runtime" and an "OCI
// sandbox" stay apart however obviously they belong together. Factoring the
// matrix replaces terms with a few hundred latent directions built out of
// terms that co-occur, and the two land in the same place without sharing a
// single word.
//
// The decomposition is randomized, following Halko, Martinsson and Tropp: a
// small random sketch of the matrix captures its dominant subspace, which is
// far cheaper than a full SVD and accurate enough at this rank. It stays
// entirely offline: nothing about it leaves the machine.
type LSA struct {
	// Base produces the term matrix that gets factored.
	Base *TFIDF
	// Dims is the number of latent dimensions to keep.
	Dims int
	// Oversample widens the random sketch. A sketch of exactly Dims columns
	// captures the leading subspace poorly; a handful of extra columns costs
	// almost nothing and markedly improves the result.
	Oversample int
	// Power is the number of power iterations. Each one pushes the sketch
	// further towards the dominant singular directions, which matters because
	// a term-document matrix has a slowly decaying spectrum.
	Power int
	Seed  int64
}

func (l *LSA) Name() string { return fmt.Sprintf("lsa(%d)", l.Dims) }

func (l *LSA) Embed(ctx context.Context, docs []Doc) (*Space, error) {
	base, err := l.Base.Embed(ctx, docs)
	if err != nil {
		return nil, err
	}
	n, d := len(base.Rows), base.Dim
	k := l.Dims
	if k <= 0 {
		k = 200
	}
	// Nothing to gain from projecting into a space as large as the original.
	if limit := min(n, d); k >= limit {
		if limit <= 2 {
			return base, nil
		}
		k = limit - 1
	}

	sketch := k + max(l.Oversample, 1)
	if sketch > min(n, d) {
		sketch = min(n, d)
	}

	// Y = A·Omega, a random projection of the documents, refined by power
	// iterations Y <- A·(A^T·Y). Re-orthonormalizing between iterations stops
	// every column collapsing onto the leading singular direction.
	rng := rand.New(rand.NewSource(l.Seed))
	y := timesDense(base.Rows, gaussian(rng, d, sketch), n)
	for i := 0; i < max(l.Power, 0); i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		q, err := orthonormal(y)
		if err != nil {
			return nil, err
		}
		y = timesDense(base.Rows, transposeTimesDense(base.Rows, q, d), n)
	}
	q, err := orthonormal(y)
	if err != nil {
		return nil, err
	}

	// With Q an orthonormal basis for the sketch, A is approximately Q·B where
	// B = Q^T·A. The singular values of B are those of A, and B·B^T is small
	// enough to decompose directly.
	b := transposeTimesDense(base.Rows, q, d) // d x sketch, that is B^T
	var c mat.Dense
	c.Mul(b.T(), b) // sketch x sketch, symmetric
	vecs, vals, err := eigen(&c)
	if err != nil {
		return nil, err
	}

	// Document coordinates are U·Sigma = Q·Vecs·Sigma, with the components
	// taken in descending order of singular value.
	cols, _ := vecs.Dims()
	if k > cols {
		k = cols
	}
	rows := make([]Vector, n)
	idx := make([]int32, k)
	for j := range idx {
		idx[j] = int32(j)
	}
	qr, _ := q.Dims()
	if qr != n {
		return nil, fmt.Errorf("internal: sketch has %d rows, expected %d", qr, n)
	}
	for i := 0; i < n; i++ {
		val := make([]float32, k)
		for j := 0; j < k; j++ {
			var s float64
			for t := 0; t < cols; t++ {
				s += q.At(i, t) * vecs.At(t, j)
			}
			val[j] = float32(s * vals[j])
		}
		rows[i] = normalizeInto(append([]int32(nil), idx...), val)
	}
	return &Space{Dim: k, Rows: rows, Backend: l.Name()}, nil
}

// orthonormal returns an orthonormal basis for the columns of y.
//
// It goes through the Gram matrix rather than a QR factorization because
// gonum's QR yields the full n x n orthogonal factor, which for a corpus of a
// few thousand repositories is a matrix hundreds of megabytes wide and almost
// entirely unwanted. Y^T·Y is only as wide as the sketch.
func orthonormal(y *mat.Dense) (*mat.Dense, error) {
	n, c := y.Dims()
	var g mat.Dense
	g.Mul(y.T(), y)
	vecs, vals, err := eigen(&g)
	if err != nil {
		return nil, err
	}

	// Directions with no energy left in them are numerical noise, and dividing
	// by their vanishing scale would amplify exactly that.
	keep := 0
	for keep < len(vals) && vals[keep] > vals[0]*1e-7 {
		keep++
	}
	if keep == 0 {
		return nil, fmt.Errorf("the term matrix has no usable structure to factor")
	}

	out := mat.NewDense(n, keep, nil)
	for i := 0; i < n; i++ {
		for j := 0; j < keep; j++ {
			var s float64
			for t := 0; t < c; t++ {
				s += y.At(i, t) * vecs.At(t, j)
			}
			out.Set(i, j, s/vals[j])
		}
	}
	return out, nil
}

// eigen decomposes a symmetric positive semi-definite matrix and returns its
// eigenvectors as columns with the square roots of its eigenvalues, both
// ordered by descending eigenvalue. gonum reports them ascending.
func eigen(m *mat.Dense) (*mat.Dense, []float64, error) {
	r, _ := m.Dims()
	sym := mat.NewSymDense(r, nil)
	for i := 0; i < r; i++ {
		for j := i; j < r; j++ {
			sym.SetSym(i, j, (m.At(i, j)+m.At(j, i))/2)
		}
	}
	var eig mat.EigenSym
	if !eig.Factorize(sym, true) {
		return nil, nil, fmt.Errorf("eigendecomposition did not converge")
	}
	asc := eig.Values(nil)
	var vecs mat.Dense
	eig.VectorsTo(&vecs)

	out := mat.NewDense(r, r, nil)
	scale := make([]float64, r)
	for j := 0; j < r; j++ {
		src := r - 1 - j
		scale[j] = math.Sqrt(math.Max(asc[src], 0))
		for i := 0; i < r; i++ {
			out.Set(i, j, vecs.At(i, src))
		}
	}
	return out, scale, nil
}

// timesDense computes A·M for sparse A given as normalized rows.
func timesDense(rows []Vector, m *mat.Dense, n int) *mat.Dense {
	_, c := m.Dims()
	out := mat.NewDense(n, c, nil)
	for i, row := range rows {
		for k, ix := range row.Idx {
			v := float64(row.Val[k])
			for j := 0; j < c; j++ {
				out.Set(i, j, out.At(i, j)+v*m.At(int(ix), j))
			}
		}
	}
	return out
}

// transposeTimesDense computes A^T·M for sparse A given as normalized rows.
func transposeTimesDense(rows []Vector, m *mat.Dense, d int) *mat.Dense {
	_, c := m.Dims()
	out := mat.NewDense(d, c, nil)
	for i, row := range rows {
		for k, ix := range row.Idx {
			v := float64(row.Val[k])
			for j := 0; j < c; j++ {
				out.Set(int(ix), j, out.At(int(ix), j)+v*m.At(i, j))
			}
		}
	}
	return out
}

func gaussian(rng *rand.Rand, rows, cols int) *mat.Dense {
	data := make([]float64, rows*cols)
	for i := range data {
		data[i] = rng.NormFloat64()
	}
	return mat.NewDense(rows, cols, data)
}
