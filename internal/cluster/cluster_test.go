package cluster

import (
	"context"
	"strings"
	"testing"

	"github.com/mrueg/constellation/internal/embed"
	"github.com/mrueg/constellation/internal/textproc"
)

// corpus is three obviously distinct groups plus two repositories that belong
// to neither, which is what a real star list looks like in miniature.
var corpus = []struct {
	name, desc, lang string
	topics           []string
	group            string
}{
	{"kubernetes/kubectl", "command line tool for kubernetes clusters", "Go", []string{"kubernetes", "cluster"}, "k8s"},
	{"argoproj/argo-cd", "declarative gitops for kubernetes", "Go", []string{"kubernetes", "gitops", "cluster"}, "k8s"},
	{"kubernetes-sigs/kustomize", "customization of kubernetes yaml configurations", "Go", []string{"kubernetes", "cluster"}, "k8s"},
	{"helm/helm", "the kubernetes package manager for clusters", "Go", []string{"kubernetes", "cluster"}, "k8s"},
	{"k3s-io/k3s", "lightweight kubernetes cluster distribution", "Go", []string{"kubernetes", "cluster"}, "k8s"},

	{"psf/requests", "an elegant http library for python", "Python", []string{"python", "http"}, "py"},
	{"pallets/flask", "the python http micro framework", "Python", []string{"python", "http"}, "py"},
	{"encode/httpx", "a next generation http client for python", "Python", []string{"python", "http"}, "py"},
	{"aio-libs/aiohttp", "asynchronous http client and server for python", "Python", []string{"python", "http"}, "py"},
	{"urllib3/urllib3", "python http client with connection pooling", "Python", []string{"python", "http"}, "py"},

	{"rust-lang/rustlings", "small exercises to learn the rust compiler", "Rust", []string{"rust", "compiler"}, "rust"},
	{"rust-lang/rust-analyzer", "rust compiler frontend for ides", "Rust", []string{"rust", "compiler"}, "rust"},
	{"bytecodealliance/wasmtime", "a rust runtime for webassembly compiler output", "Rust", []string{"rust", "compiler"}, "rust"},
	{"denoland/deno", "a rust runtime built on the compiler toolchain", "Rust", []string{"rust", "compiler"}, "rust"},
	{"tokio-rs/tokio", "asynchronous rust runtime and compiler support", "Rust", []string{"rust", "compiler"}, "rust"},

	{"someone/recipes", "grandmother's cooking notes", "", []string{"cooking"}, "odd"},
	{"someone/birdwatching", "notes about garden birds", "", []string{"birds"}, "odd"},
}

func build(t *testing.T) ([]textproc.Doc, *embed.Space) {
	t.Helper()
	docs := make([]textproc.Doc, len(corpus))
	for i, c := range corpus {
		owner, name, _ := strings.Cut(c.name, "/")
		docs[i] = textproc.Build(owner, name, c.desc, c.lang, c.topics, "")
	}
	sp, err := (&embed.TFIDF{MinDF: 1, MaxDFRatio: 0.6, MaxVocab: 5000}).Embed(context.Background(), docs)
	if err != nil {
		t.Fatal(err)
	}
	return docs, sp
}

func TestClusteringRecoversTopics(t *testing.T) {
	docs, sp := build(t)
	res := Run(sp, Options{MinK: 2, MaxK: 6, Restarts: 4, MaxIter: 60, Seed: 1, SilhouetteSample: 0})
	res = Refine(sp, res, RefineOptions{MinSize: 3, MinSimilarity: 0.05, OutlierSigmas: 3})

	if res.K != 3 {
		t.Errorf("K = %d, want the 3 planted groups", res.K)
	}
	// Every member of a planted group must land in the same cluster.
	byGroup := map[string]map[int]bool{}
	for i, c := range corpus {
		if c.group == "odd" {
			continue
		}
		if byGroup[c.group] == nil {
			byGroup[c.group] = map[int]bool{}
		}
		byGroup[c.group][res.Assign[i]] = true
	}
	for g, clusters := range byGroup {
		if len(clusters) != 1 {
			t.Errorf("group %q was split across %d clusters", g, len(clusters))
		}
		for c := range clusters {
			if c < 0 {
				t.Errorf("group %q was left unassigned", g)
			}
		}
	}

	labels := Names(docs, res.Assign, res.K, 6)
	var names []string
	for _, l := range labels {
		names = append(names, l.Name)
	}
	joined := strings.ToLower(strings.Join(names, " "))
	for _, want := range []string{"kubernetes", "python", "rust"} {
		if !strings.Contains(joined, want) {
			t.Errorf("cluster names %v do not mention %q", names, want)
		}
	}
}

func TestRefineLeavesMisfitsAlone(t *testing.T) {
	_, sp := build(t)
	res := Run(sp, Options{K: 3, Restarts: 4, MaxIter: 60, Seed: 1})
	res = Refine(sp, res, RefineOptions{MinSize: 3, MinSimilarity: 0.05, OutlierSigmas: 3})
	for i, c := range corpus {
		if c.group == "odd" && res.Assign[i] >= 0 {
			t.Errorf("%s was forced into cluster %d despite fitting nothing", c.name, res.Assign[i])
		}
	}
}

func TestDeterministic(t *testing.T) {
	_, sp := build(t)
	opts := Options{MinK: 2, MaxK: 6, Restarts: 3, MaxIter: 60, Seed: 7, SilhouetteSample: 0}
	a := Refine(sp, Run(sp, opts), RefineOptions{MinSize: 3})
	b := Refine(sp, Run(sp, opts), RefineOptions{MinSize: 3})
	for i := range a.Assign {
		if a.Assign[i] != b.Assign[i] {
			t.Fatalf("same seed produced different assignments at %d: %d vs %d", i, a.Assign[i], b.Assign[i])
		}
	}
}

func TestNamesAreDistinctAndFitGitHub(t *testing.T) {
	docs, sp := build(t)
	res := Refine(sp, Run(sp, Options{K: 3, Restarts: 3, MaxIter: 60, Seed: 1}), RefineOptions{MinSize: 3})
	seen := map[string]bool{}
	for _, l := range Names(docs, res.Assign, res.K, 6) {
		if l.Name == "" {
			t.Error("empty cluster name")
		}
		if len(l.Name) > MaxListName {
			t.Errorf("name %q exceeds GitHub's %d character limit", l.Name, MaxListName)
		}
		if seen[strings.ToLower(l.Name)] {
			t.Errorf("duplicate cluster name %q", l.Name)
		}
		seen[strings.ToLower(l.Name)] = true
		if d := Describe(l); len(d) > MaxListDescription {
			t.Errorf("description %q exceeds %d characters", d, MaxListDescription)
		}
	}
}
