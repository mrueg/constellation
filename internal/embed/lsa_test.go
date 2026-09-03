package embed

import (
	"context"
	"math"
	"testing"

	"github.com/mrueg/constellation/internal/textproc"
	"gonum.org/v1/gonum/mat"
)

// docsFrom builds documents from descriptions alone. Owner and repository name
// are left out deliberately: both are unique per document and weighted above
// the description, so including them would make every document mostly about
// its own name and leave nothing for the factorization to find.
func docsFrom(descs []string) []Doc {
	docs := make([]Doc, len(descs))
	for i, d := range descs {
		docs[i] = textproc.Build("", "", d, "", nil, "")
	}
	return docs
}

func baseTFIDF() *TFIDF { return &TFIDF{MinDF: 1, MaxDFRatio: 1, MaxVocab: 5000} }

// bridged is two vocabularies with no word in common — and none the alias
// table quietly unifies, which would make the test pass for the wrong reason —
// joined by documents that use both, plus an unrelated topic to be far from.
var bridged = []string{
	"sandbox isolation jail", "sandbox jail isolation",
	"namespace cgroup seccomp", "cgroup namespace seccomp",
	"sandbox isolation namespace cgroup", "jail sandbox seccomp namespace",
	"isolation jail cgroup seccomp", "sandbox namespace cgroup jail",
	"isolation seccomp sandbox namespace", "jail cgroup isolation namespace",
	"poetry sonnet rhyme verse", "sonnet poetry verse rhyme",
	"rhyme poetry sonnet stanza", "verse stanza rhyme poetry",
}

// This is the whole reason LSA is here: term matching scores two documents
// about the same thing at exactly zero when they share no vocabulary, and
// factoring the matrix repairs that.
//
// The merge comes from truncation. Keeping only the leading components forces
// terms that co-occur through the bridging documents onto shared directions;
// keep more components and the two vocabularies separate again, which is why
// this asserts at a deliberately low rank.
func TestLSABridgesDisjointVocabulary(t *testing.T) {
	const (
		alpha = 0  // "sandbox isolation jail"
		gamma = 2  // "namespace cgroup seccomp" — no word in common with alpha
		far   = 13 // unrelated topic
	)
	docs := docsFrom(bridged)
	tf := baseTFIDF()

	lexical, err := tf.Embed(context.Background(), docs)
	if err != nil {
		t.Fatal(err)
	}
	if before := lexical.Rows[alpha].DotSparse(lexical.Rows[gamma]); before > 1e-9 {
		t.Fatalf("fixture is wrong: the two documents share vocabulary (%.4f)", before)
	}

	latent, err := (&LSA{Base: tf, Dims: 2, Oversample: 6, Power: 2, Seed: 1}).
		Embed(context.Background(), docs)
	if err != nil {
		t.Fatal(err)
	}
	related := latent.Rows[alpha].DotSparse(latent.Rows[gamma])
	unrelated := latent.Rows[alpha].DotSparse(latent.Rows[far])
	if related < 0.9 {
		t.Errorf("disjoint-but-bridged documents scored %.4f, want them brought together", related)
	}
	// And it must not simply make everything similar to everything.
	if unrelated > 0.1 {
		t.Errorf("unrelated document scored %.4f, want it left apart", unrelated)
	}
}

// The randomized decomposition is an approximation, so it is pinned against
// the exact answer: a dense SVD of the same matrix, which is affordable at
// this size but not at a real corpus's.
func TestLSAMatchesExactSVD(t *testing.T) {
	docs := docsFrom(bridged)
	tf := baseTFIDF()
	base, err := tf.Embed(context.Background(), docs)
	if err != nil {
		t.Fatal(err)
	}
	n, d := len(base.Rows), base.Dim

	dense := mat.NewDense(n, d, nil)
	for i, r := range base.Rows {
		for k, ix := range r.Idx {
			dense.Set(i, int(ix), float64(r.Val[k]))
		}
	}
	var svd mat.SVD
	if !svd.Factorize(dense, mat.SVDThin) {
		t.Fatal("reference SVD did not converge")
	}
	sv := svd.Values(nil)
	var u mat.Dense
	svd.UTo(&u)

	for _, k := range []int{2, 3, 4} {
		got, err := (&LSA{Base: tf, Dims: k, Oversample: 6, Power: 2, Seed: 1}).
			Embed(context.Background(), docs)
		if err != nil {
			t.Fatal(err)
		}
		// Compare the geometry rather than the coordinates: a singular vector
		// is only defined up to sign, so pairwise cosines are what must agree.
		for _, pair := range [][2]int{{0, 2}, {0, 13}, {4, 5}, {2, 3}} {
			want := exactCosine(u, sv, k, pair[0], pair[1])
			have := got.Rows[pair[0]].DotSparse(got.Rows[pair[1]])
			if math.Abs(want-have) > 1e-4 {
				t.Errorf("k=%d docs %v: randomized SVD gave %.6f, exact SVD %.6f", k, pair, have, want)
			}
		}
	}
}

func exactCosine(u mat.Dense, sv []float64, k, x, y int) float64 {
	var dot, nx, ny float64
	for j := 0; j < k; j++ {
		a, b := u.At(x, j)*sv[j], u.At(y, j)*sv[j]
		dot += a * b
		nx += a * a
		ny += b * b
	}
	if nx == 0 || ny == 0 {
		return 0
	}
	return dot / math.Sqrt(nx*ny)
}

func TestLSAIsDeterministicAndNormalized(t *testing.T) {
	docs := docsFrom([]string{
		"kubernetes cluster operator", "kubernetes helm chart",
		"rust compiler runtime", "rust cargo crate",
		"python http client", "python asyncio server",
	})
	tf := baseTFIDF()
	mk := func() *Space {
		sp, err := (&LSA{Base: tf, Dims: 3, Oversample: 4, Power: 2, Seed: 7}).
			Embed(context.Background(), docs)
		if err != nil {
			t.Fatal(err)
		}
		return sp
	}
	a, b := mk(), mk()
	for i := range a.Rows {
		// Coordinates are stored as float32, so agreement is only ever to
		// single precision.
		if got := a.Rows[i].DotSparse(b.Rows[i]); got < 1-1e-6 {
			t.Errorf("row %d differs between runs with the same seed (cosine %.9f)", i, got)
		}
		if norm := a.Rows[i].DotSparse(a.Rows[i]); math.Abs(norm-1) > 1e-6 {
			t.Errorf("row %d has norm %.6f, want 1", i, norm)
		}
	}
}

// Asking for more dimensions than the data holds must degrade gracefully.
func TestLSAClampsDimensions(t *testing.T) {
	docs := docsFrom([]string{"one two", "three four", "five six"})
	sp, err := (&LSA{Base: baseTFIDF(), Dims: 500, Oversample: 10, Power: 1, Seed: 1}).
		Embed(context.Background(), docs)
	if err != nil {
		t.Fatal(err)
	}
	if sp.Dim > len(docs) {
		t.Errorf("Dim = %d, want it clamped to at most %d", sp.Dim, len(docs))
	}
}
