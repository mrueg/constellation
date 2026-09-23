package textproc

import (
	"reflect"
	"strings"
	"testing"
)

func TestTokenize(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"go-github", []string{"github"}},
		{"HTTPServerPool", []string{"http", "server", "pool"}},
		// Terms are stems: grouping keys, not labels. Names are built from
		// the surface forms recorded alongside them.
		{"k8s_operator", []string{"kubernetes", "oper"}},
		{"the best awesome list of tools", nil},
		{"parsers", []string{"parser"}},
		{"kubernetes", []string{"kubernetes"}},
		{"v1.2.3", nil},
	} {
		if got := Tokenize(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Tokenize(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestBuildRanksOwnerBelowSubject(t *testing.T) {
	d := Build("google", "bumble", "a bluetooth stack", "Python", []string{"bluetooth"}, "")
	owner, topic, name := term("google"), term("bluetooth"), term("bumble")
	if d.Terms[owner] >= d.Terms[topic] || d.Terms[owner] >= d.Terms[name] {
		t.Fatalf("owner should rank below the topic and the repository name: %v", d.Terms)
	}
}

// term is the stem a word contributes, for tests that care about weights
// rather than about the stemmer.
func term(w string) string {
	got := Tokenize(w)
	if len(got) != 1 {
		panic("expected a single term for " + w)
	}
	return got[0]
}

// Stemming exists so that wordings of the same thing share a term.
func TestVariantsShareATerm(t *testing.T) {
	for _, group := range [][]string{
		{"monitoring", "monitors", "monitor"},
		{"analytics", "analytic"},
		{"exporters", "exporter"},
	} {
		first := term(group[0])
		for _, w := range group[1:] {
			if got := term(w); got != first {
				t.Errorf("%q gives %q but %q gives %q; they should group together", group[0], first, w, got)
			}
		}
	}
}

// The stem is for grouping; the label must still read as a real word.
func TestSurfaceKeepsTheReadableForm(t *testing.T) {
	d := Build("acme", "thing", "analytics for monitoring clusters", "", nil, "")
	for _, want := range []string{"analytics", "monitoring"} {
		if got := d.Surface[term(want)]; got != want {
			t.Errorf("surface for %q = %q, want %q", want, got, want)
		}
	}
}

// A stem that would collide with a curated name must be left alone: the
// stemmer turns "kubernetes" into "kubernet" and "macos" into "maco".
func TestCuratedTermsAreNotStemmed(t *testing.T) {
	for _, w := range []string{"kubernetes", "macos", "prometheus"} {
		if got := term(w); got != w {
			t.Errorf("term(%q) = %q, want it left intact", w, got)
		}
	}
	// A language word is exempt for the same reason, via its namespace.
	if got := term("typescript"); got != LangPrefix+"typescript" {
		t.Errorf("term(\"typescript\") = %q, want the namespaced language term", got)
	}
}

func TestBuildWeightsTopicsAboveProse(t *testing.T) {
	d := Build("acme", "thing", "a thing for widgets", "Go", []string{"widgets"}, "")
	if d.Terms["widget"] <= d.Terms["thing"] {
		t.Fatalf("topic term should outweigh a description term: %v", d.Terms)
	}
}

// The Language field used to vanish for exactly the languages that mattered
// most: "go" and "js" are stopwords in prose, and "JavaScript" camel-split
// into "java" and "script", pulling JavaScript repositories towards Java.
func TestLanguageAlwaysProducesATerm(t *testing.T) {
	// The detected language is not counted by default, so the mechanism is
	// exercised with an explicit weight.
	w := DefaultWeights()
	w.Language = 1.5
	for _, lang := range []string{"Go", "JavaScript", "C++", "C#", "Jupyter Notebook", "Zig", "Rust"} {
		d := BuildWith(w, nil, "acme", "thing", "a thing", lang, nil, "")
		want := Language(lang)
		if want == "" {
			t.Fatalf("Language(%q) produced no term", lang)
		}
		if d.Terms[want] == 0 {
			t.Errorf("Build with Language=%q produced %v, want a %q term", lang, d.Terms, want)
		}
	}
}

// C, C++ and C# must not collapse onto each other.
func TestLanguageKeepsTheCFamilyApart(t *testing.T) {
	seen := map[string]string{}
	for _, lang := range []string{"C", "C++", "C#", "F#", "Objective-C"} {
		got := Language(lang)
		if prev, dup := seen[got]; dup {
			t.Errorf("%q and %q both produced %q", prev, lang, got)
		}
		seen[got] = lang
	}
}

// Prose must not be able to fake a language term.
// A language the author tagged as a topic still counts; only the detected one
// is ignored.
func TestTaggedLanguageStillCounts(t *testing.T) {
	if d := Build("a", "b", "", "Go", nil, ""); len(d.Terms) != 0 {
		for term := range d.Terms {
			if strings.HasPrefix(term, LangPrefix) {
				t.Errorf("the detected language should not be counted by default, got %q", term)
			}
		}
	}
	if d := Build("a", "b", "", "", []string{"golang"}, ""); d.Terms[LangPrefix+"go"] == 0 {
		t.Errorf("a tagged language should still count: %v", d.Terms)
	}
}

func TestProseDoesNotProduceALanguageTerm(t *testing.T) {
	d := Build("acme", "thing", "go and see how it works, then go back", "", nil, "")
	for term := range d.Terms {
		if strings.HasPrefix(term, LangPrefix) {
			t.Errorf("description produced the language term %q", term)
		}
	}
}

// A topic naming a language unambiguously should reinforce the Language field
// rather than land in a separate bucket.
func TestLanguageTopicsJoinTheLanguageTerm(t *testing.T) {
	w := DefaultWeights()
	w.Language = 1.5
	field := BuildWith(w, nil, "a", "b", "", "Go", nil, "")
	topic := Build("a", "b", "", "", []string{"golang"}, "")
	for term := range topic.Terms {
		if field.Terms[term] == 0 {
			t.Errorf("topic \"golang\" produced %q, which the Language field does not", term)
		}
	}
}

// Two topics can reduce to the same pair of stems, and which one the index
// keeps must not depend on map iteration order — that made the whole pipeline
// non-deterministic.
func TestTopicIndexResolvesCollisionsDeterministically(t *testing.T) {
	lists := [][]string{
		{"kubernetes-cluster"}, {"kubernetes-cluster"}, {"kubernetes-cluster"},
		{"k8s-cluster"}, {"k8s-cluster"}, {"k8s-cluster"}, {"k8s-cluster"},
	}
	var first TopicIndex
	for i := 0; i < 20; i++ {
		idx := NewTopicIndex(lists, 3)
		if first == nil {
			first = idx
		}
		for k, v := range idx {
			if first[k] != v {
				t.Fatalf("index differs between builds at %q: %q vs %q", k, first[k], v)
			}
		}
	}
	// The more used spelling wins.
	if got := first["kubernetes cluster"]; got != "k8s-cluster" {
		t.Errorf("collision resolved to %q, want the more frequent \"k8s-cluster\"", got)
	}
}

// A README's opening badge line is markup and URLs, and every one of its
// fragments is shared by thousands of unrelated repositories.
func TestTokenizeDropsBadgesAndURLs(t *testing.T) {
	badge := `[![Build](https://img.shields.io/badge/build-passing-green.svg)](https://github.com/foo/bar)`
	if got := Tokenize(badge); len(got) != 0 {
		t.Errorf("Tokenize(badge) = %v, want nothing", got)
	}
	noise := map[string]bool{"https": true, "http": true, "img": true, "svg": true, "shield": true, "shields": true, "io": true, "com": true, "badg": true, "badge": true}
	for _, in := range []string{
		badge,
		"see https://example.com/docs/index.html for details",
		"visit www.example.com/foo to read more",
		`<p align="center"><img src="logo.png" width="200"></p> a widget factory`,
		"[the documentation](https://docs.example.io) explains widgets",
	} {
		for _, term := range Tokenize(in) {
			if noise[term] {
				t.Errorf("Tokenize(%q) produced the URL fragment %q", in, term)
			}
		}
	}
	// Link text is prose and stays; only the target goes.
	if got := Tokenize("[the documentation](https://docs.example.io) explains widgets"); !reflect.DeepEqual(got, []string{"document", "explain", "widget"}) {
		t.Errorf("link text was not kept: %v", got)
	}
	// Words that are only ever README boilerplate are gone too.
	if got := Tokenize("Installation Usage License MIT Apache Contributing Changelog"); len(got) != 0 {
		t.Errorf("README boilerplate survived: %v", got)
	}
}

// An alias is applied to prose as well as topics, so it has to be
// unambiguous in a sentence. These four were not.
func TestAmbiguousAliasesLeaveProseAlone(t *testing.T) {
	got := Tokenize("cd into the directory and run obs studio with vim keybindings; crypto trading bot; dl the file")
	for _, bad := range []string{"cicd", "observ", "observability", "neovim", "cryptographi", "cryptography", "deeplearning"} {
		for _, term := range got {
			if term == bad {
				t.Errorf("prose produced the aliased term %q: %v", bad, got)
			}
		}
	}
	found := false
	for _, term := range got {
		if term == "vim" {
			found = true
		}
	}
	if !found {
		t.Errorf("vim should survive as itself: %v", got)
	}
	// The unambiguous ones still collapse.
	if got := Tokenize("k8s ci pipelines"); !reflect.DeepEqual(got, []string{"kubernetes", "cicd", "pipelin"}) {
		t.Errorf("Tokenize(k8s ci pipelines) = %v", got)
	}
}

// Vim and Neovim plugins are one ecosystem and the list is called "Vim".
func TestVimSpellingsShareATerm(t *testing.T) {
	for _, w := range []string{"vim", "nvim", "neovim", "Neovim"} {
		if got := term(w); got != "vim" {
			t.Errorf("term(%q) = %q, want \"vim\"", w, got)
		}
	}
	if Pretty["vim"] != "Vim" || Pretty["neovim"] != "Neovim" {
		t.Errorf("Pretty renders vim as %q and neovim as %q", Pretty["vim"], Pretty["neovim"])
	}
}

// The splitter drops punctuation, which used to erase exactly the names made
// of it: "C++ library for .NET" came out as "net".
func TestPunctuatedNamesSurvive(t *testing.T) {
	got := Tokenize("C++ library for .NET and Node.js; C# bindings and an F# port")
	want := []string{"cpp", "dotnet", "nodejs", "csharp", "bind", "fsharp", "port"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Tokenize = %v, want %v", got, want)
	}
	// Case-insensitive and word-bounded: "libc++" is not C++ and "asp.netcore"
	// is not .NET, while ".net" at the end of a sentence is.
	if got := Tokenize("written in c++ and c#"); !reflect.DeepEqual(got, []string{"cpp", "csharp"}) {
		t.Errorf("lower-cased spellings: %v", got)
	}
	if got := Tokenize("ported to .NET"); !reflect.DeepEqual(got, []string{"port", "dotnet"}) {
		t.Errorf(".NET at the end: %v", got)
	}
	for _, in := range []string{"libc++ headers", "asp.netcore host"} {
		for _, term := range Tokenize(in) {
			if term == "cpp" || term == "dotnet" {
				t.Errorf("Tokenize(%q) produced %q from a partial match", in, term)
			}
		}
	}
	for term, want := range map[string]string{"cpp": "C++", "csharp": "C#", "fsharp": "F#", "dotnet": ".NET", "nodejs": "Node.js"} {
		if Pretty[term] != want {
			t.Errorf("Pretty[%q] = %q, want %q", term, Pretty[term], want)
		}
	}
	// The Language field goes through the same table.
	for lang, want := range map[string]string{"C++": "cpp", "C#": "csharp", "F#": "fsharp", "Objective-C++": "objectivecpp"} {
		if got := Language(lang); got != LangPrefix+want {
			t.Errorf("Language(%q) = %q, want %q", lang, got, LangPrefix+want)
		}
	}
}
