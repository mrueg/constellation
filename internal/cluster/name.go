package cluster

import (
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/mrueg/constellation/internal/textproc"
)

// Label is a proposed name for a cluster, together with the evidence behind it
// so a human can judge — and override — the machine's choice.
type Label struct {
	Name      string
	TopTerms  []string
	TermScore []float64
	// Lead is the raw term behind the first of TopTerms, before it was made
	// readable. Anything that needs to ask how many members actually carry
	// what the category is named after needs the term, not its rendering.
	Lead string
}

// MaxListName and MaxListDescription mirror the limits the GitHub star list
// form enforces.
const (
	MaxListName        = 32
	MaxListDescription = 160
)

// bigramBoost is how much a two-word phrase is preferred over the single words
// it contains when they score alike.
const bigramBoost = 1.35

// topicBoost is how much a term earns for having come from repository topics
// rather than from prose. Topics are labels somebody applied deliberately, and
// GitHub suggests them from a curated set, so they are both more canonical and
// less noisy than the words in a description.
const topicBoost = 1.6

// topicBonus scales between one and topicBoost according to how much of a
// term's weight in this cluster came from topics, so a term used both ways is
// credited in proportion rather than all or nothing.
func topicBonus(topicWeight, total float64) float64 {
	if total <= 0 || topicWeight <= 0 {
		return 1
	}
	share := topicWeight / total
	if share > 1 {
		share = 1
	}
	return 1 + (topicBoost-1)*share
}

// notASubject lists words that describe how a project is run rather than what
// it is about. They are left in the vectors, where they do group repositories
// that genuinely share a culture, but a list called "Hacktoberfest" tells its
// reader nothing — the cluster it named here also contained mob programming,
// Kubernetes and Gentoo.
var notASubject = map[string]bool{
	"hacktoberfest": true, "oss": true, "opensource": true, "sourceopen": true,
	"goodfirstissue": true, "helpwanted": true, "firsttimersonly": true,
	"contribution": true, "contributor": true, "starred": true, "star": true,
	"todo": true, "wip": true, "misc": true, "stuff": true, "various": true,
}

func subjectless(display string) bool {
	return notASubject[strings.ToLower(strings.ReplaceAll(display, " ", ""))]
}

// Names labels every cluster using class-based TF-IDF: a term describes a
// cluster well when it is frequent inside it and rare in the others. Plain
// frequency would name half the clusters after whatever dominates the account.
func Names(docs []textproc.Doc, assign []int, k int, topN int) []Label {
	if topN <= 0 {
		topN = 6
	}
	perCluster := make([]map[string]float64, k)
	totals := make([]float64, k)
	global := map[string]float64{}
	for c := range perCluster {
		perCluster[c] = map[string]float64{}
	}
	fromTopic := make([]map[string]float64, k)
	for c := range fromTopic {
		fromTopic[c] = map[string]float64{}
	}
	accumulate := func(c int, src map[string]float64, boost float64) {
		for term, w := range src {
			w *= boost
			perCluster[c][term] += w
			totals[c] += w
			global[term] += w
		}
	}
	for i, d := range docs {
		c := assign[i]
		if c < 0 || c >= k {
			continue
		}
		accumulate(c, d.Terms, 1)
		for term, w := range d.FromTopic {
			fromTopic[c][term] += w
		}
		// A phrase scores close to its own parts, since they largely co-occur.
		// The boost tips near-ties towards the more specific label, so a
		// category comes out as "Self Hosted" rather than "Self & Hosted".
		accumulate(c, d.Bigrams, bigramBoost)
	}
	display := displayForms(docs)
	labels := make([]Label, k)
	used := map[string]bool{}
	for c := 0; c < k; c++ {
		type scored struct {
			term  string
			score float64
		}
		var ranked []scored
		for term, w := range perCluster[c] {
			if totals[c] == 0 || global[term] == 0 {
				continue
			}
			// A term names a cluster well when it is both common inside it and
			// rare outside it. Frequency alone names every cluster after the
			// corpus-wide favourite; exclusivity alone names them after
			// whatever one-off word happens to be unique, which is how a Rust
			// cluster ends up called "Tokio".
			frequency := w / totals[c]
			exclusivity := w / global[term]
			ranked = append(ranked, scored{term, frequency * exclusivity * topicBonus(fromTopic[c][term], w)})
		}
		sort.Slice(ranked, func(i, j int) bool {
			if ranked[i].score != ranked[j].score {
				return ranked[i].score > ranked[j].score
			}
			return ranked[i].term < ranked[j].term
		})

		// Labels carry readable words, not the stems used for grouping.
		// Distinct terms are collected up to topN rather than the top topN
		// being filtered afterwards, so that discarding one never leaves the
		// list short.
		//
		// Two different terms can render identically: a topic kept whole and
		// the bigram of its own words both read as "Home Assistant". They are
		// one piece of evidence counted twice, so only the stronger is kept.
		l := Label{}
		seen := map[string]bool{}
		for _, s := range ranked {
			d := display[s.term]
			if d == "" || subjectless(d) || seen[strings.ToLower(d)] {
				continue
			}
			seen[strings.ToLower(d)] = true
			if l.Lead == "" {
				l.Lead = s.term
			}
			l.TopTerms = append(l.TopTerms, d)
			l.TermScore = append(l.TermScore, s.score)
			if len(l.TopTerms) == topN {
				break
			}
		}
		l.Name = composeName(l.TopTerms, l.TermScore, used)
		used[strings.ToLower(l.Name)] = true
		labels[c] = l
	}
	return labels
}

// displayForms resolves every stem to the wording it is most often written as
// across the corpus, so a cluster grouped on "analyt" is labelled "Analytics"
// if that is how the repositories in it actually spell it. Ties break
// alphabetically to keep the whole pipeline deterministic.
func displayForms(docs []textproc.Doc) map[string]string {
	counts := map[string]map[string]int{}
	casings := map[string]map[string]int{}
	for _, d := range docs {
		for term, surface := range d.Surface {
			if counts[term] == nil {
				counts[term] = map[string]int{}
			}
			counts[term][surface]++
		}
		for word, written := range d.Cased {
			if casings[word] == nil {
				casings[word] = map[string]int{}
			}
			casings[word][written]++
		}
	}
	cased := make(map[string]string, len(casings))
	for word, written := range casings {
		if c := distinctiveCasing(written); c != "" {
			cased[word] = c
		}
	}
	out := make(map[string]string, len(counts))
	for term, surfaces := range counts {
		out[term] = pretty(mostCommon(surfaces, term), cased)
	}
	return out
}

// distinctiveCasing reports how the corpus writes a word, but only when that
// tells us something a capitalization rule could not.
//
// Prose writes ordinary words in lower case because they appear mid-sentence,
// so taking the majority spelling outright produced categories called
// "container" and "security". What is worth keeping is the unusual shape —
// "eBPF", "gRPC", "PostgreSQL", "MCP" — which is exactly a capital letter
// somewhere other than the front. It also has to be the clear majority, or a
// single description writing "PI" renames Raspberry Pi.
func distinctiveCasing(counts map[string]int) string {
	total := 0
	for _, n := range counts {
		total += n
	}
	best, bestN := "", 0
	for v, n := range counts {
		if n > bestN || (n == bestN && v < best) {
			best, bestN = v, n
		}
	}
	if bestN < 3 || bestN*2 <= total {
		return ""
	}
	for _, r := range []rune(best)[1:] {
		if unicode.IsUpper(r) {
			return best
		}
	}
	return ""
}

// mostCommon picks the most frequent spelling, breaking ties alphabetically so
// the pipeline stays deterministic.
func mostCommon(counts map[string]int, fallback string) string {
	best, bestN := fallback, -1
	for v, n := range counts {
		if n > bestN || (n == bestN && v < best) {
			best, bestN = v, n
		}
	}
	return best
}

// composeName turns the top terms into a short list title, joining a second
// term only when it carries comparable weight to the first.
func composeName(terms []string, scores []float64, used map[string]bool) string {
	if len(terms) == 0 {
		return "Uncategorized"
	}
	// A phrase built around the leading term beats gluing two terms together
	// with an ampersand: the cluster whose terms were "Actions, GitHub, GitHub
	// Actions" was being called "Actions & GitHub".
	if phrase := phraseAround(terms, scores); phrase != "" && !used[strings.ToLower(phrase)] {
		return phrase
	}
	parts := []string{terms[0]}
	for i := 1; i < len(terms) && len(parts) < 3; i++ {
		if scores[i] < 0.45*scores[0] || isPhrase(terms[0]) {
			break
		}
		p := terms[i]
		if redundant(parts, p) {
			continue
		}
		if len(strings.Join(append(parts, p), " & ")) > MaxListName {
			break
		}
		parts = append(parts, p)
		if !used[strings.ToLower(strings.Join(parts, " & "))] {
			break // two terms are usually enough; keep going only to break a tie
		}
	}
	name := strings.Join(parts, " & ")
	name = truncate(name, MaxListName)
	// Distinct names matter: GitHub rejects duplicates, and so does a reader.
	if used[strings.ToLower(name)] {
		for i := 2; ; i++ {
			suffix := fmt.Sprint(i)
			cand := name + " " + suffix
			if len([]rune(cand)) > MaxListName {
				cand = truncate(name, MaxListName-len([]rune(suffix))-1) + " " + suffix
			}
			if !used[strings.ToLower(cand)] {
				return cand
			}
		}
	}
	return name
}

// redundant reports whether p repeats something already in the name, which is
// common once phrases are in play: "Kubernetes" and "Kubernetes Operator"
// should not both appear.
// phraseAround returns a candidate phrase that contains the leading term, if
// one scores well enough to be worth preferring over it.
func phraseAround(terms []string, scores []float64) string {
	top := strings.ToLower(terms[0])
	for i := 1; i < len(terms); i++ {
		if !isPhrase(terms[i]) || scores[i] < 0.25*scores[0] {
			continue
		}
		for _, w := range strings.Fields(strings.ToLower(terms[i])) {
			if w == top && len(terms[i]) <= MaxListName {
				return terms[i]
			}
		}
	}
	return ""
}

func redundant(parts []string, p string) bool {
	lp := strings.ToLower(p)
	for _, e := range parts {
		le := strings.ToLower(e)
		if strings.Contains(le, lp) || strings.Contains(lp, le) {
			return true
		}
		for _, w := range strings.Fields(lp) {
			if strings.Contains(le, w) {
				return true
			}
		}
	}
	return false
}

// pretty renders a term for display, preferring the curated spelling where one
// exists and title-casing otherwise. A phrase is rendered word by word.
func pretty(term string, cased map[string]string) string {
	if p, ok := textproc.Pretty[term]; ok {
		return p
	}
	// A language term the table does not name explicitly still reads fine
	// once its namespace prefix is removed: "lang:zig" is just "Zig".
	if rest, ok := strings.CutPrefix(term, textproc.LangPrefix); ok {
		return pretty(rest, cased)
	}
	// A topic reads as its words: "home-assistant" is "Home Assistant".
	if rest, ok := strings.CutPrefix(term, textproc.TopicPrefix); ok {
		return pretty(strings.ReplaceAll(rest, "-", " "), cased)
	}
	if isPhrase(term) {
		words := strings.Split(term, " ")
		for i, w := range words {
			words[i] = pretty(w, cased)
		}
		return strings.Join(words, " ")
	}
	// How the corpus itself writes the word beats any capitalization rule:
	// title-casing produces "Mcp", "Grpc" and "Postgresql".
	if written, ok := cased[term]; ok {
		return written
	}
	r := []rune(term)
	if len(r) == 0 {
		return term
	}
	return string(unicode.ToUpper(r[0])) + string(r[1:])
}

func isPhrase(term string) bool { return strings.Contains(term, " ") }

// DescriptionMarker closes every description this tool writes, so that a list
// it manages can be told from one made by hand. Reconciling deletes lists and
// empties them, and it must never touch a list somebody curated themselves.
//
// It sits at the end rather than the front so the description reads as a
// description, with the attribution as a footnote.
const DescriptionMarker = "Grouped by constellation."

// Describe writes a list description from what distinguishes the category.
//
// It names the subject rather than listing members: a reader of "Repositories
// about Helm, charts and plugins" learns what belongs there, where a couple of
// example repositories only say what happens to be in it already — and go
// stale the moment anything is added.
func Describe(l Label) string {
	var terms []string
	for i, t := range l.TopTerms {
		if len(l.TermScore) > i && l.TermScore[i] < 0.2*l.TermScore[0] {
			break
		}
		if !contains(terms, t) {
			terms = append(terms, t)
		}
		if len(terms) == 4 {
			break
		}
	}
	if len(terms) == 0 {
		return DescriptionMarker
	}
	subject := terms[0]
	if len(terms) > 1 {
		subject = strings.Join(terms[:len(terms)-1], ", ") + " and " + terms[len(terms)-1]
	}
	d := "Repositories about " + subject + ". " + DescriptionMarker
	if len(d) > MaxListDescription {
		// Drop terms until it fits rather than cutting a word in half.
		for len(terms) > 1 && len(d) > MaxListDescription {
			terms = terms[:len(terms)-1]
			subject = terms[0]
			if len(terms) > 1 {
				subject = strings.Join(terms[:len(terms)-1], ", ") + " and " + terms[len(terms)-1]
			}
			d = "Repositories about " + subject + ". " + DescriptionMarker
		}
	}
	// The ellipsis is three bytes and one column; budget it in runes, or the
	// result overruns the limit it was cut to fit.
	if len([]rune(d)) > MaxListDescription {
		d = truncate(d, MaxListDescription-1) + "…"
	}
	return d
}

func contains(ss []string, s string) bool {
	for _, e := range ss {
		if e == s {
			return true
		}
	}
	return false
}

// truncate cuts a string to at most n characters.
//
// Counting bytes instead splits a multi-byte rune in half and posts invalid
// UTF-8 to GitHub, and it also cuts non-ASCII names roughly twice as short as
// necessary, since GitHub's own limits are in characters.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(string(r[:n]))
}
