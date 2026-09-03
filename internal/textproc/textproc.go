// Package textproc turns repository metadata into the bag-of-words
// representation the embedding and naming stages both work from.
package textproc

import (
	"strings"
	"unicode"

	"github.com/kljensen/snowball/english"
)

// Doc is one tokenized repository. Terms are weighted: a term that came from
// the repo's curated topic list counts for more than one that came from prose,
// because topics are hand-applied labels while descriptions are marketing copy.
type Doc struct {
	Terms map[string]float64
	// Bigrams holds adjacent term pairs, kept apart from Terms so they inform
	// naming without inflating the vectors. A category is far better described
	// by "self hosted" or "home assistant" than by either half of it.
	Bigrams map[string]float64
	// Surface maps each stem back to a form it was actually written as. Stems
	// group well but read terribly — "analytics" and "analytic" both stem to
	// "analyt", which no one wants to see on a list — so the vector space uses
	// the stem and the label uses the word.
	Surface map[string]string
	// FromTopic records how much of a term's weight came from the repository's
	// topics rather than from prose. Topics are labels somebody applied on
	// purpose; a description is marketing copy. When naming a category the
	// difference is worth a lot, so the provenance is carried through.
	FromTopic map[string]float64
	// Cased maps a lower-cased word to the capitalization the corpus writes it
	// with. GitHub lower-cases every topic, so this can only come from
	// descriptions and repository names — but there it is unambiguous:
	// "eBPF" outnumbers "ebpf" by four to one, and "gRPC" and "PostgreSQL"
	// are spellings no capitalization rule would ever produce.
	Cased map[string]string
}

// Field weights. Topics dominate, then the name (owner/repo often encodes the
// ecosystem), then language, then free text.
const (
	WeightTopic = 3.0
	// A topic's words are worth less than the topic itself: the label is the
	// deliberate part, its vocabulary is shared with everything else.
	WeightTopicWord = 1.2
	// Weights for a topic recognized in text rather than applied by a person,
	// graded by how reliably each source names the project.
	//
	// Measured against repositories that do carry topics, a phrase found in
	// the repository name is really one of its topics 65% of the time, in the
	// description 43%, and in the opening of the README 33% — falling to 21%
	// by a hundred words in. Precision tracks how deliberately the text was
	// written: a name is chosen once and carefully, a description is a
	// sentence, a README is prose that mentions neighbouring technologies
	// without being about them. None of these approaches a real topic, so none
	// is weighted like one.
	WeightTopicFromName   = 3.0
	WeightTopicFromText   = 2.0
	WeightTopicFromReadme = 1.0
	// ReadmeInferWords is how far into a README a topic phrase is still
	// trusted. Precision halves between the opening and the hundred-word mark.
	ReadmeInferWords = 20
	WeightName       = 2.0
	// The detected primary language is deliberately not counted as evidence.
	// Every repository in a language shares one identical term, which makes it
	// an overwhelmingly strong grouper that then absorbs whatever else is
	// nearby — it was fusing Go with Wardley mapping, and C with Zsh. Measured
	// on a real account, dropping it raised topic agreement by a tenth and
	// halved the number of categories named after two unrelated things. A
	// language the author tagged as a topic still counts, at topic weight,
	// because that is a choice rather than a detection.
	WeightLanguage = 0
	WeightText     = 1.0
	// An owner is a publisher, not a subject. Weighting it like the repository
	// name produces a "Google" category holding a fuzzer, a Bluetooth stack
	// and a YAML formatter. It still earns a small weight because narrow
	// owners — kubernetes, rust-lang, bufbuild — really do say what a
	// repository is about.
	WeightOwner = 0.5
	// A README is a hundred terms against a description's nine, so per term it
	// has to count for much less if the two are to carry comparable weight.
	WeightReadme = 0.15
)

var stopwords = buildStopwords()

// buildStopwords is a plain initializer rather than an init function: it
// depends on nothing but the literal below, so there is no ordering to reason
// about and the value can be seen at its declaration.
func buildStopwords() map[string]bool {
	const list = `a an and are as at be by for from has have how in into is it its of on or that the
to was were will with you your this these those we our us they them he she his her not no but if
then than there here when where which who whom whose what why all any both each few more most other
some such only own same so too very can just should now about above after again against because
before being below between during further off once out over under until while do does did doing
library libraries lib package packages module modules project projects repo repository repositories
tool tools toolkit simple easy fast small tiny lightweight modern minimal awesome list collection
curated based written using use used uses using support supports supported implementation code
source open free new go golang js javascript version like also via etc example examples demo
best great powerful flexible full complete unofficial official yet another cross platform`
	set := make(map[string]bool)
	for _, w := range strings.Fields(list) {
		set[w] = true
	}
	return set
}

// aliases collapse the many spellings of the same ecosystem onto one term so
// that "k8s" and "kubernetes" repos land in the same cluster.
var aliases = map[string]string{
	"k8s": "kubernetes", "kube": "kubernetes", "kubectl": "kubernetes",
	"ml": "machinelearning", "ai": "artificialintelligence", "dl": "deeplearning",
	"llm": "llm", "llms": "llm", "genai": "generativeai",
	// These name a language unambiguously wherever they appear — most often as
	// a repository topic — so they join the term the Language field produces
	// instead of being filtered out as prose. Bare "go" stays a stopword: it
	// is a verb far more often than it is a language.
	"golang": LangPrefix + "go", "javascript": LangPrefix + "javascript",
	"js": LangPrefix + "javascript", "typescript": LangPrefix + "typescript",
	"ts": LangPrefix + "typescript", "py": LangPrefix + "python",
	"python": LangPrefix + "python", "rs": LangPrefix + "rust",
	"rust": LangPrefix + "rust", "rustlang": LangPrefix + "rust",
	"k3s": "kubernetes", "oci": "container", "docker": "container",
	"containers": "container", "containerd": "container",
	"cli": "commandline", "tui": "terminal", "cmdline": "commandline",
	"ci": "cicd", "cd": "cicd", "devops": "cicd",
	"db": "database", "sql": "database", "dbs": "database",
	"sec": "security", "infosec": "security", "appsec": "security",
	"k6": "loadtesting", "obs": "observability", "otel": "opentelemetry",
	"prom": "prometheus", "graf": "grafana",
	"nvim": "neovim", "vim": "neovim",
	"web3": "blockchain", "crypto": "cryptography",
	"gh": "github", "gitops": "gitops",
}

// Pretty renders a canonical term back into something a human wants to read on
// a list. Only entries that title-casing gets wrong need to appear here.
var Pretty = map[string]string{
	"kubernetes": "Kubernetes", "machinelearning": "Machine Learning",
	"artificialintelligence": "AI", "deeplearning": "Deep Learning",
	"generativeai": "Generative AI", "llm": "LLM", "commandline": "CLI",
	"cicd": "CI/CD", "javascript": "JavaScript", "typescript": "TypeScript",
	"opentelemetry": "OpenTelemetry", "graphql": "GraphQL", "postgresql": "PostgreSQL",
	"postgres": "Postgres", "nosql": "NoSQL", "grpc": "gRPC", "api": "API",
	"apis": "APIs", "sdk": "SDK", "http": "HTTP", "dns": "DNS", "tls": "TLS",
	"aws": "AWS", "gcp": "GCP", "css": "CSS", "html": "HTML", "json": "JSON",
	"yaml": "YAML", "wasm": "WebAssembly", "ebpf": "eBPF", "gpu": "GPU",
	"os": "OS", "rss": "RSS", "vpn": "VPN", "ssh": "SSH", "ui": "UI", "ux": "UX",
	"neovim": "Neovim", "nixos": "NixOS", "macos": "macOS", "ios": "iOS",
	"imap": "IMAP", "smtp": "SMTP", "jmap": "JMAP", "pop3": "POP3", "mta": "MTA",
	"csv": "CSV", "xml": "XML", "ntp": "NTP", "smb": "SMB", "nfs": "NFS",
	"esp32": "ESP32", "esp8266": "ESP8266", "rfc": "RFC", "pdf": "PDF", "svg": "SVG",
	"oidc": "OIDC", "saml": "SAML", "ldap": "LDAP", "mqtt": "MQTT", "cve": "CVE",
	"sbom": "SBOM", "tui": "TUI", "ide": "IDE", "vm": "VM", "cpu": "CPU",
	"github": "GitHub", "gitlab": "GitLab", "gitops": "GitOps", "devsecops": "DevSecOps",
	"nodejs": "Node.js", "dotnet": ".NET", "cpp": "C++", "csharp": "C#",
	LangPrefix + "go": "Go", LangPrefix + "cpp": "C++", LangPrefix + "csharp": "C#",
	LangPrefix + "fsharp": "F#", LangPrefix + "c": "C", LangPrefix + "objectivec": "Objective-C",
	LangPrefix + "javascript": "JavaScript", LangPrefix + "typescript": "TypeScript",
	LangPrefix + "jupyternotebook": "Jupyter Notebook", LangPrefix + "vimscript": "Vim Script",
	LangPrefix + "emacslisp": "Emacs Lisp", LangPrefix + "powershell": "PowerShell",
	LangPrefix + "shell": "Shell", LangPrefix + "php": "PHP", LangPrefix + "sql": "SQL",
	LangPrefix + "html": "HTML", LangPrefix + "css": "CSS", LangPrefix + "scss": "SCSS",
	LangPrefix + "ocaml": "OCaml", LangPrefix + "matlab": "MATLAB", LangPrefix + "tex": "TeX",
	LangPrefix + "hcl": "HCL", LangPrefix + "rmarkdown": "R Markdown", LangPrefix + "r": "R",
}

// LangPrefix namespaces the term derived from a repository's Language field.
//
// Without it the language signal is quietly lost for exactly the languages
// that matter most: "go" and "js" have to be stopwords because they are common
// English words and abbreviations in prose, and "JavaScript" camel-splits into
// "java" and "script", which pulls JavaScript repositories towards Java ones.
// A namespaced token cannot collide with prose, so it never needs filtering.
const LangPrefix = "lang:"

// TopicPrefix namespaces a repository topic kept whole.
//
// Thirty per cent of topic assignments are multi-word, and splitting them
// destroys what made them worth the most. "machine-learning" is a label chosen
// from a curated set; "machin" and "learn" are two common words that collide
// with any description mentioning learning. The label is kept as one term so
// that two repositories carrying it match exactly, and its words are kept too,
// at lower weight, so a repository describing machine learning in prose can
// still match approximately.
//
// Single-word topics are left alone: for those the decomposed form already is
// the whole label, and isolating it would only stop it matching prose.
const TopicPrefix = "topic:"

// TopicIndex maps the stemmed word pair a multi-word topic reduces to onto the
// topic itself, so that the same phrase written in prose can be recognized.
//
// It is built from the topics the corpus already uses, which makes it a closed
// vocabulary: a description can be matched against labels other repositories
// have chosen, but no new label can be invented. Only multi-word topics are
// indexed — "security", "cli" and "api" occur in prose constantly and matching
// those would label everything.
type TopicIndex map[string]string

// NewTopicIndex builds the index from every repository's topics, keeping those
// used at least minCount times.
func NewTopicIndex(topicLists [][]string, minCount int) TopicIndex {
	counts := map[string]int{}
	for _, list := range topicLists {
		for _, t := range list {
			if canonical := CanonicalTopic(t); canonical != "" {
				counts[canonical]++
			}
		}
	}
	// Different topics can reduce to the same pair of stems — "k8s-cluster"
	// and "kubernetes-cluster" both become "kubernetes cluster" — so the
	// collision has to be resolved on purpose. The most used spelling wins,
	// alphabetically on a tie. Leaving it to whichever the map yielded last
	// made the whole pipeline non-deterministic, because a different label
	// meant a different term and so a different clustering.
	best := map[string]string{}
	for canonical, n := range counts {
		if n < minCount {
			continue
		}
		words := Tokenize(canonical)
		if len(words) != 2 {
			continue
		}
		key := words[0] + " " + words[1]
		if held, ok := best[key]; !ok || n > counts[held] || (n == counts[held] && canonical < held) {
			best[key] = canonical
		}
	}
	return TopicIndex(best)
}

// Language turns a GitHub language name into its namespaced term. The slug has
// to keep C, C++ and C# apart, which stripping punctuation would not.
func Language(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return ""
	}
	name = strings.ReplaceAll(name, "++", "pp")
	name = strings.ReplaceAll(name, "#", "sharp")
	var b strings.Builder
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return LangPrefix + b.String()
}

// token pairs the stem a phrase contributes to the vector space with the form
// it was written as.
// token carries three forms of one word: the stem that groups it, the
// lower-cased word behind that stem, and the casing it was actually written
// with. The last is what makes a category read as "MCP" rather than "Mcp".
type token struct {
	term, surface, original string
	// raw is the word exactly as written, before an alias may have rewritten
	// it into another word entirely. "AI" becomes the term
	// "artificialintelligence", and without keeping the original spelling
	// there is nothing left to tell anyone it is written "AI" and not "Ai".
	raw string
}

// Tokenize splits a phrase into normalized terms, expanding camelCase and
// snake/kebab boundaries so "go-github" and "goGitHub" agree.
func Tokenize(s string) []string {
	var out []string
	for _, t := range tokenize(s) {
		out = append(out, t.term)
	}
	return out
}

func tokenize(s string) []token {
	var out []token
	for _, chunk := range strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		// A known term is kept whole. Splitting on case would otherwise turn
		// "TypeScript" into "type" and "script", and a category named
		// "Type & Script" helps nobody.
		if t, ok := normalize(chunk); ok && known(strings.ToLower(chunk)) {
			out = append(out, t)
			continue
		}
		for _, w := range splitCamel(chunk) {
			if t, ok := normalize(w); ok {
				out = append(out, t)
			}
		}
	}
	return out
}

func normalize(w string) (token, bool) {
	raw := w
	w = strings.ToLower(w)
	if len(w) < 2 || len(w) > 24 {
		return token{}, false
	}
	if a, ok := aliases[w]; ok {
		w = a
	}
	if stopwords[w] || isNumeric(w) || isVersion(w) {
		return token{}, false
	}
	t := token{term: stem(w), surface: w, raw: raw}
	// Only a pure difference in case counts as the same word written
	// differently. An alias rewrote the word into another one entirely, and
	// its spelling says nothing about how that other word is written.
	if strings.EqualFold(raw, w) {
		t.original = raw
	}
	return t, true
}

// stem reduces a word to the form used for grouping, so that "monitoring",
// "monitors" and "monitor" land on one term instead of three.
//
// Curated and namespaced terms are exempt. The stemmer is a general-purpose
// English algorithm and mangles the proper nouns this corpus is full of —
// "kubernetes" becomes "kubernet", "macos" becomes "maco" — which is harmless
// for grouping but not for the tables that key off the exact word.
func stem(w string) string {
	if known(w) || strings.HasPrefix(w, LangPrefix) {
		return w
	}
	return english.Stem(w, false)
}

// isVersion drops "v1", "v2" and the fragments that version strings leave
// behind; they group repositories by nothing at all.
func isVersion(w string) bool {
	return w[0] == 'v' && isNumeric(w[1:])
}

func isNumeric(w string) bool {
	if w == "" {
		return false
	}
	for _, r := range w {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// CanonicalTopic normalizes a topic into the single term representing it.
func CanonicalTopic(t string) string {
	t = strings.ToLower(strings.TrimSpace(t))
	var b strings.Builder
	dash := false
	for _, r := range t {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimRight(b.String(), "-")
}

// snapshot copies the current term weights so that what a topic contributed
// can be told apart from what prose did.
func snapshot(m map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// distinctive reports whether a spelling carries a capital anywhere but the
// front, which is the shape no capitalization rule would produce: "eBPF",
// "gRPC", "MCP".
func distinctive(w string) bool {
	for _, r := range []rune(w)[1:] {
		if unicode.IsUpper(r) {
			return true
		}
	}
	return false
}

// known reports whether a term is one the tables already recognize, and so
// should survive normalization untouched.
func known(w string) bool {
	if _, ok := aliases[w]; ok {
		return true
	}
	_, ok := Pretty[w]
	return ok
}

func splitCamel(s string) []string {
	var out []string
	start := 0
	rs := []rune(s)
	for i := 1; i < len(rs); i++ {
		prev, cur := rs[i-1], rs[i]
		boundary := unicode.IsLower(prev) && unicode.IsUpper(cur)
		if i+1 < len(rs) && unicode.IsUpper(prev) && unicode.IsUpper(cur) && unicode.IsLower(rs[i+1]) {
			boundary = true // "HTTPServer" -> "HTTP", "Server"
		}
		if boundary {
			out = append(out, string(rs[start:i]))
			start = i
		}
	}
	return append(out, string(rs[start:]))
}

// Weights control how much each part of a repository record counts. They
// matter more than they look: a README contributes a hundred or so terms and a
// description contributes nine, so an even-handed weighting is not even-handed
// at all — it lets prose drown the curated topics that carry the most signal
// per term.
type Weights struct {
	// Topic is the weight of a multi-word topic kept whole, and of a
	// single-word topic, which amounts to the same thing.
	Topic float64
	// TopicWord is the weight of each word a multi-word topic decomposes into.
	TopicWord float64
	// TopicFromName, TopicFromText and TopicFromReadme weight a topic
	// recognized in each of those, rather than applied to the repository.
	TopicFromName   float64
	TopicFromText   float64
	TopicFromReadme float64
	Name            float64
	Language        float64
	Text            float64
	Owner           float64
	Readme          float64
}

// DefaultWeights are the ones the CLI uses unless told otherwise.
func DefaultWeights() Weights {
	return Weights{
		Topic:           WeightTopic,
		TopicWord:       WeightTopicWord,
		TopicFromName:   WeightTopicFromName,
		TopicFromText:   WeightTopicFromText,
		TopicFromReadme: WeightTopicFromReadme,
		Name:            WeightName,
		Language:        WeightLanguage,
		Text:            WeightText,
		Owner:           WeightOwner,
		Readme:          WeightReadme,
	}
}

// Build assembles a weighted Doc using the default weights.
func Build(owner, name, description, language string, topics []string, readme string) Doc {
	return BuildWith(DefaultWeights(), nil, owner, name, description, language, topics, readme)
}

// BuildWith assembles a weighted Doc from the parts of a repository record.
func BuildWith(w Weights, index TopicIndex, owner, name, description, language string, topics []string, readme string) Doc {
	d := Doc{
		Terms:     map[string]float64{},
		Bigrams:   map[string]float64{},
		Surface:   map[string]string{},
		Cased:     map[string]string{},
		FromTopic: map[string]float64{},
	}
	note := func(term, surface string) {
		if _, seen := d.Surface[term]; !seen {
			d.Surface[term] = surface
		}
	}
	add := func(s string, weight float64) {
		// A term recorded with zero weight is not the same as no term: the
		// vectoriser takes the logarithm of it, and log(0) poisons the whole
		// matrix.
		if weight <= 0 {
			return
		}
		toks := tokenize(s)
		for i, t := range toks {
			d.Terms[t.term] += weight
			note(t.term, t.surface)
			// Prefer the more distinctive spelling. A repository name is read
			// before its description and is almost always lower case, so
			// first-seen would record "mcp" and never see the "MCP" the
			// description writes a line later.
			noteCase := func(key, written string) {
				if written == "" {
					return
				}
				if prev, seen := d.Cased[key]; !seen || (!distinctive(prev) && distinctive(written)) {
					d.Cased[key] = written
				}
			}
			noteCase(t.surface, t.original)
			// Also under the word as written, so that an aliased word keeps a
			// spelling of its own for display.
			noteCase(strings.ToLower(t.raw), t.raw)
			if i > 0 && toks[i-1].term != t.term {
				bigram := toks[i-1].term + " " + t.term
				d.Bigrams[bigram] += weight
				note(bigram, toks[i-1].surface+" "+t.surface)
			}
		}
	}
	for _, t := range topics {
		before := snapshot(d.Terms)
		// With a topic weight of zero the label is not kept whole and the
		// topic behaves as it used to, entirely as its words — which is what
		// makes the old behaviour measurable rather than merely remembered.
		whole := w.Topic > 0 && len(Tokenize(t)) > 1
		if whole {
			if canonical := CanonicalTopic(t); canonical != "" {
				term := TopicPrefix + canonical
				d.Terms[term] += w.Topic
				note(term, term)
			} else {
				whole = false
			}
		}
		switch {
		case whole:
			add(t, w.TopicWord)
		case w.Topic > 0:
			add(t, w.Topic)
		default:
			add(t, w.TopicWord)
		}
		for term, now := range d.Terms {
			if now > before[term] {
				d.FromTopic[term] += now - before[term]
			}
		}
	}
	add(owner, w.Owner)
	add(name, w.Name)
	if lang := Language(language); lang != "" && w.Language > 0 {
		d.Terms[lang] += w.Language
		note(lang, lang)
		d.FromTopic[lang] = 0 // a language is a fact, not a curated label
	}
	// A repository nobody labelled can still be recognized from what it says
	// about itself, against the labels its neighbours use. This is only done
	// when there are no topics at all: where somebody has already chosen
	// labels, guessing more from prose would only add noise to a better signal.
	//
	// Each source is weighted by how often it turns out to be right, so the
	// reach of a README can be used without trusting it like a name.
	if len(topics) == 0 && len(index) > 0 {
		infer := func(text string, weight float64, limit int) {
			if weight <= 0 || text == "" {
				return
			}
			toks := Tokenize(text)
			if limit > 0 && len(toks) > limit {
				toks = toks[:limit]
			}
			for i := 1; i < len(toks); i++ {
				canonical, ok := index[toks[i-1]+" "+toks[i]]
				if !ok {
					continue
				}
				term := TopicPrefix + canonical
				d.Terms[term] += weight
				note(term, term)
				d.FromTopic[term] += weight
			}
		}
		infer(name, w.TopicFromName, 0)
		infer(description, w.TopicFromText, 0)
		infer(readme, w.TopicFromReadme, ReadmeInferWords)
	}
	add(description, w.Text)
	add(readme, w.Readme)
	return d
}
