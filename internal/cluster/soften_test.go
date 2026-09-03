package cluster

import (
	"context"
	"strings"
	"testing"

	"github.com/mrueg/constellation/internal/embed"
	"github.com/mrueg/constellation/internal/textproc"
)

// planted builds a corpus with groups of deliberately unequal size, which is
// what separates the algorithms: k-means pulls towards equal spheres, while
// modularity does not.
func planted(t *testing.T, groups [][]string) (*embed.Space, []int) {
	t.Helper()
	var docs []embed.Doc
	var truth []int
	for g, descs := range groups {
		for _, d := range descs {
			docs = append(docs, textproc.Build("", "", d, "", nil, ""))
			truth = append(truth, g)
		}
	}
	sp, err := (&embed.TFIDF{MinDF: 1, MaxDFRatio: 0.9, MaxVocab: 5000}).
		Embed(context.Background(), docs)
	if err != nil {
		t.Fatal(err)
	}
	return sp, truth
}

func repeated(s string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = s
	}
	return out
}

// agrees reports whether every member of each planted group shares one cluster.
func agrees(res *Result, truth []int, groups int) bool {
	for g := 0; g < groups; g++ {
		seen := map[int]bool{}
		for i, t := range truth {
			if t == g {
				seen[res.Assign[i]] = true
			}
		}
		if len(seen) != 1 {
			return false
		}
	}
	return true
}

var unevenGroups = [][]string{
	append(repeated("kubernetes cluster operator helm", 30), "kubernetes cluster operator chart"),
	append(repeated("rust cargo compiler crate", 12), "rust cargo compiler binary"),
	{"sonnet rhyme verse stanza", "sonnet rhyme verse meter", "sonnet rhyme stanza meter", "sonnet verse stanza meter"},
}

// Soft membership is judged on centroid similarity, not on mixture
// posteriors, which saturate to zero and one in any real number of dimensions.
func TestSoftenPlacesStraddlingRepositoriesInBothLists(t *testing.T) {
	sp, _ := planted(t, [][]string{
		repeated("kubernetes cluster operator", 8),
		repeated("rust cargo compiler", 8),
		repeated("rust kubernetes operator cargo cluster compiler", 4),
	})
	res := Run(sp, Options{K: 2, Restarts: 3, MaxIter: 60, Seed: 1})
	res = Soften(sp, res, 0.8, 2)

	both := 0
	for i := 16; i < 20; i++ {
		if len(res.Also[i]) > 0 {
			both++
		}
	}
	if both == 0 {
		t.Error("no repository using both vocabularies was placed in both lists")
	}
	// The unambiguous ones must stay in one list.
	if len(res.Also[0]) > 0 || len(res.Also[8]) > 0 {
		t.Errorf("a single-topic repository was placed in several lists: %v / %v", res.Also[0], res.Also[8])
	}
}

// A ratio of one keeps the partition untouched; that is the default.
func TestSoftenIsOffByDefault(t *testing.T) {
	sp, _ := planted(t, unevenGroups)
	res := Soften(sp, Run(sp, Options{K: 3, Restarts: 2, MaxIter: 60, Seed: 1}), 0.8, 1)
	for i, also := range res.Also {
		if len(also) > 0 {
			t.Fatalf("repository %d joined extra lists with maxLists=1: %v", i, also)
		}
	}
}

// Members must report secondary memberships, or soft assignment would compute
// but never reach a list.
func TestMembersIncludesSecondaryMemberships(t *testing.T) {
	r := &Result{
		Assign: []int{0, 1, 0},
		Also:   [][]int{nil, {0}, nil},
		K:      2,
	}
	got := r.Members(0)
	want := []int{0, 1, 2}
	if len(got) != len(want) {
		t.Fatalf("Members(0) = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("Members(0) = %v, want %v", got, want)
		}
	}
}

// Capitalization is learned from how the corpus writes a word, because no
// rule produces "gRPC" or "PostgreSQL" — and title-casing produces "Mcp".
func TestNamesUseTheCorpusCapitalization(t *testing.T) {
	var docs []textproc.Doc
	for i := 0; i < 6; i++ {
		docs = append(docs, textproc.Build("", "", "an MCP server for gRPC and PostgreSQL", "", nil, ""))
	}
	for i := 0; i < 6; i++ {
		docs = append(docs, textproc.Build("", "", "sonnet rhyme verse stanza", "", nil, ""))
	}
	assign := make([]int, len(docs))
	for i := range assign {
		if i >= 6 {
			assign[i] = 1
		}
	}
	labels := Names(docs, assign, 2, 8)
	joined := strings.Join(append(labels[0].TopTerms, labels[1].TopTerms...), " ")
	for _, want := range []string{"MCP", "gRPC", "PostgreSQL"} {
		if !strings.Contains(joined, want) {
			t.Errorf("terms %q do not contain %q", joined, want)
		}
	}
	for _, bad := range []string{"Mcp", "Grpc", "Postgresql"} {
		if strings.Contains(joined, bad) {
			t.Errorf("terms %q contain the title-cased %q", joined, bad)
		}
	}
}

// "Actions & GitHub" was wrong when "GitHub Actions" was already a candidate.
func TestPhraseAroundTopTermWins(t *testing.T) {
	terms := []string{"Actions", "GitHub", "GitHub Actions", "Runner"}
	scores := []float64{1.0, 0.9, 0.8, 0.4}
	if got := phraseAround(terms, scores); got != "GitHub Actions" {
		t.Errorf("phraseAround = %q, want \"GitHub Actions\"", got)
	}
	// A phrase that does not contain the leading term must not be chosen.
	if got := phraseAround([]string{"Helm", "Chart"}, []float64{1, 0.9}); got != "" {
		t.Errorf("phraseAround = %q, want none", got)
	}
	// Nor a phrase too weak to be worth preferring.
	if got := phraseAround(terms, []float64{1.0, 0.9, 0.1, 0.4}); got != "" {
		t.Errorf("phraseAround = %q, want none for a weak phrase", got)
	}
}

// A participation label is not a subject and must not name a list.
func TestProcessLabelsDoNotName(t *testing.T) {
	if !subjectless("Hacktoberfest") || !subjectless("Good First Issue") {
		t.Error("participation labels should be excluded from naming")
	}
	if subjectless("Kubernetes") || subjectless("Home Assistant") {
		t.Error("real subjects must not be excluded")
	}
}

// A topic kept whole and the bigram of its own words render identically, so a
// term list could show "Home Assistant, Home Assistant".
func TestNamesDoNotRepeatATerm(t *testing.T) {
	var docs []textproc.Doc
	for i := 0; i < 8; i++ {
		docs = append(docs, textproc.Build("", "", "home assistant integration", "",
			[]string{"home-assistant", "smart-home"}, ""))
	}
	for i := 0; i < 8; i++ {
		docs = append(docs, textproc.Build("", "", "sonnet rhyme verse", "", []string{"poetry"}, ""))
	}
	assign := make([]int, len(docs))
	for i := 8; i < len(docs); i++ {
		assign[i] = 1
	}
	for _, l := range Names(docs, assign, 2, 6) {
		seen := map[string]bool{}
		for _, term := range l.TopTerms {
			key := strings.ToLower(term)
			if seen[key] {
				t.Errorf("term %q repeated in %v", term, l.TopTerms)
			}
			seen[key] = true
		}
	}
}

// Dropping a term must not leave the list short of what was asked for.
func TestNamesFillUpToTopN(t *testing.T) {
	var docs []textproc.Doc
	for i := 0; i < 10; i++ {
		docs = append(docs, textproc.Build("", "", "kubernetes cluster operator helm chart registry", "",
			[]string{"hacktoberfest", "kubernetes"}, ""))
	}
	for i := 0; i < 10; i++ {
		docs = append(docs, textproc.Build("", "", "sonnet rhyme verse stanza meter", "", []string{"poetry"}, ""))
	}
	assign := make([]int, len(docs))
	for i := 10; i < len(docs); i++ {
		assign[i] = 1
	}
	l := Names(docs, assign, 2, 4)[0]
	if len(l.TopTerms) != 4 {
		t.Errorf("got %d terms %v, want 4 despite a stoplisted one being dropped", len(l.TopTerms), l.TopTerms)
	}
}

// Ward linkage has no random element, so the same input must give exactly the
// same answer — that is the whole reason to offer it.
func TestAgglomerativeIsDeterministic(t *testing.T) {
	sp, truth := planted(t, unevenGroups)
	a := Agglomerate(sp, 3)
	b := Agglomerate(sp, 3)
	for i := range a.Assign {
		if a.Assign[i] != b.Assign[i] {
			t.Fatalf("two runs disagreed at %d: %d vs %d", i, a.Assign[i], b.Assign[i])
		}
	}
	if !agrees(a, truth, len(unevenGroups)) {
		t.Errorf("planted groups were split: %v", a.Assign)
	}
}

func TestAgglomerativeCutsToRequestedCount(t *testing.T) {
	sp, _ := planted(t, unevenGroups)
	for _, k := range []int{2, 3, 5} {
		if got := Agglomerate(sp, k); got.K != k {
			t.Errorf("Agglomerate(k=%d) produced %d clusters", k, got.K)
		}
	}
}

// Cutting a dendrogram at a fixed height can separate one category into two.
func TestMergeDuplicatesFoldsARepeatedCategory(t *testing.T) {
	// The four exporter repositories are indistinguishable, so any split of
	// them is arbitrary — which is exactly the case the merge exists for.
	descs := []string{
		"prometheus exporter metrics scrape", "prometheus exporter metrics scrape",
		"prometheus exporter metrics scrape", "prometheus exporter metrics scrape",
		"sonnet rhyme verse stanza", "sonnet rhyme verse stanza",
	}
	var docs []textproc.Doc
	for _, d := range descs {
		docs = append(docs, textproc.Build("", "", d, "", nil, ""))
	}
	sp, err := (&embed.TFIDF{MinDF: 1, MaxDFRatio: 1, MaxVocab: 500}).Embed(context.Background(), docs)
	if err != nil {
		t.Fatal(err)
	}
	// Three clusters where the data holds two: the exporters are split.
	r := &Result{Assign: []int{0, 0, 1, 1, 2, 2}, K: 3, Centroids: make([][]float64, 3)}
	for c := range r.Centroids {
		r.Centroids[c] = make([]float64, sp.Dim)
	}
	recomputeCentroids(sp, r)

	got := MergeDuplicates(docs, sp, r, 0.3)
	if got.K != 2 {
		t.Errorf("got %d categories, want the two split halves folded back into 2", got.K)
	}
	// The unrelated pair must survive as its own category.
	if got.Assign[4] == got.Assign[0] {
		t.Error("an unrelated category was merged in")
	}
}

func TestMergeDuplicatesIsOffAtZero(t *testing.T) {
	sp, _ := planted(t, unevenGroups)
	var docs []textproc.Doc
	for _, g := range unevenGroups {
		for _, d := range g {
			docs = append(docs, textproc.Build("", "", d, "", nil, ""))
		}
	}
	r := Agglomerate(sp, 4)
	if got := MergeDuplicates(docs, sp, r, 0); got.K != 4 {
		t.Errorf("got %d categories, want 4 untouched", got.K)
	}
}

// A junk drawer is not detectable by coherence: vectors can be generically
// alike without being about anything. Asking how many members carry the term
// the category is named after does detect it.
func TestDropUnnamedRemovesAJunkDrawer(t *testing.T) {
	// Two real categories, plus six repositories with nothing in common.
	descs := []string{
		"prometheus exporter metrics", "prometheus exporter metrics",
		"prometheus exporter metrics", "prometheus exporter metrics",
		"sonnet rhyme verse", "sonnet rhyme verse", "sonnet rhyme verse", "sonnet rhyme verse",
		"retry backoff library", "mob programming session", "kde desktop widget",
		"debian packaging helper", "bird migration tracker", "coffee grinder timer",
	}
	var docs []textproc.Doc
	for _, d := range descs {
		docs = append(docs, textproc.Build("", "", d, "", nil, ""))
	}
	sp, err := (&embed.TFIDF{MinDF: 1, MaxDFRatio: 1, MaxVocab: 500}).Embed(context.Background(), docs)
	if err != nil {
		t.Fatal(err)
	}
	assign := make([]int, len(docs))
	for i := range assign {
		switch {
		case i < 4:
			assign[i] = 0
		case i < 8:
			assign[i] = 1
		default:
			assign[i] = 2 // the junk drawer
		}
	}
	r := &Result{Assign: assign, K: 3, Centroids: make([][]float64, 3)}
	for c := range r.Centroids {
		r.Centroids[c] = make([]float64, sp.Dim)
	}
	recomputeCentroids(sp, r)

	got := DropUnnamed(docs, sp, r, 0.5)
	if got.K != 2 {
		t.Fatalf("got %d categories, want the junk drawer dropped leaving 2", got.K)
	}
	for i := 8; i < len(descs); i++ {
		if got.Assign[i] >= 0 {
			t.Errorf("repository %d stayed categorized after its category was dissolved", i)
		}
	}
}

func TestDropUnnamedIsOffAtZero(t *testing.T) {
	sp, _ := planted(t, unevenGroups)
	var docs []textproc.Doc
	for _, g := range unevenGroups {
		for _, d := range g {
			docs = append(docs, textproc.Build("", "", d, "", nil, ""))
		}
	}
	r := Agglomerate(sp, 3)
	if got := DropUnnamed(docs, sp, r, 0); got.K != 3 {
		t.Errorf("got %d categories, want 3 untouched", got.K)
	}
}
