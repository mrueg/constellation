package gh

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// A README is reduced to the part of it that describes the project: its
// title, a tagline if it has one, and the first descriptive paragraphs.
// Everything else in a README is noise for categorization. Badges, code
// blocks and link URLs are the same in thousands of unrelated repositories — a
// shields.io URL would cluster them together — and installation, usage and
// licensing sections say the same things in every project.
//
// The reduction happens on the whole file, and only then is anything cut.
// Cutting first was the old approach, and a README that opens with a screen
// of badges, a centred HTML header and a table of contents spent the whole
// budget before its first sentence of prose.

// minSummaryWords is how long a paragraph must be to count as the README's
// summary of the project rather than a tagline, a badge caption or a stray
// line left over from stripping.
const minSummaryWords = 15

// maxTaglines bounds the short lines kept from under the title: a tagline or
// two is signal, a column of one-line notices is not.
const maxTaglines = 2

// skipHeadings names the sections every project has and that say nothing
// about what this one is. A heading is skipped when it contains one of these
// as a run of whole words, so "Installation", "Quick start guide" and
// "License (MIT)" all match while "Supported platforms" does not.
var skipHeadings = [][]string{
	{"installation"}, {"install"}, {"installing"}, {"getting", "started"}, {"setup"},
	{"usage"}, {"quick", "start"}, {"quickstart"}, {"requirements"}, {"prerequisites"},
	{"building"}, {"build"}, {"development"}, {"contributing"}, {"contributors"},
	{"license"}, {"licence"}, {"changelog"}, {"table", "of", "contents"}, {"contents"},
	{"acknowledgements"}, {"acknowledgments"}, {"credits"}, {"sponsors"}, {"support"},
	{"donate"}, {"star", "history"}, {"badges"},
}

var (
	fenced      = regexp.MustCompile("(?s)```.*?```|~~~.*?~~~")
	htmlComment = regexp.MustCompile(`(?s)<!--.*?-->`)
	htmlTag     = regexp.MustCompile(`(?s)</?[A-Za-z][^<>]*>`)
	// Images, inline or reference-style. Their alt text is a badge label far
	// more often than a description, so the whole thing goes.
	mdImage = regexp.MustCompile(`!\[[^\]]*\](\([^)]*\)|\[[^\]]*\])`)
	// Links, inline or reference-style, reduced to their text.
	mdLink  = regexp.MustCompile(`\[([^\]]*)\](\([^)]*\)|\[[^\]]*\])`)
	bareURL = regexp.MustCompile(`https?://\S+`)
	// A reference-style link definition: `[name]: https://...`.
	refDef = regexp.MustCompile(`(?m)^[ \t]{0,3}\[[^\]]+\]:[ \t]*\S.*$`)

	atxHeading = regexp.MustCompile(`^[ \t]{0,3}(#{1,6})(?:[ \t]+(.*?))?[ \t]*#*[ \t]*$`)
	setextLine = regexp.MustCompile(`^[ \t]{0,3}(=+|-+)[ \t]*$`)
	ruleLine   = regexp.MustCompile(`^[ \t]{0,3}([-*_])([ \t]*[-*_]){2,}[ \t]*$`)
	listItem   = regexp.MustCompile(`^[ \t]*(?:[-*+]|\d+[.)])[ \t]+`)
	tableRow   = regexp.MustCompile(`^[ \t]*\|`)
	tableDelim = regexp.MustCompile(`^[ \t]*\|?[ \t]*:?-{2,}:?[ \t]*(\|[ \t]*:?-{2,}:?[ \t]*)+\|?[ \t]*$`)
	// A list item made of nothing but links is a table of contents entry,
	// whether or not it sits under a heading that says so.
	navItem = regexp.MustCompile(`^[ \t]*(?:[-*+]|\d+[.)])[ \t]+(?:\[[^\]]*\]\([^)]*\)[ \t,|·]*)+$`)
	spaces  = regexp.MustCompile(`\s+`)
)

// section is one heading's worth of a README, with its prose already reduced
// to paragraphs of plain words.
type section struct {
	level int    // 1 for an H1; 0 for text before the first heading
	title string // the heading, stripped of markup
	skip  bool   // a section that says nothing about the project
	paras []string
}

// readmeOpening returns the README's own description of the project, at most
// maxWords long (zero or less keeps all of it): the H1 title and any short
// tagline under it, then the first paragraph of at least minSummaryWords from
// a section that is not installation, usage, licensing or another that every
// project has, and the paragraphs after it until the budget is spent. A
// README with no such paragraph falls back to the first paragraphs of any
// section that is not skipped; one with no headings is a single section.
//
// The text comes back collapsed to single spaces and valid UTF-8, cut only on
// word boundaries.
func readmeOpening(markdown string, maxWords int) string {
	secs := readmeSections(markdown)

	var (
		parts []string
		words int
	)
	// add appends a paragraph and reports whether the budget is now spent.
	add := func(p string) bool {
		parts = append(parts, p)
		words += wordCount(p)
		return maxWords > 0 && words >= maxWords
	}

	for _, s := range secs {
		if s.level == 1 {
			if !s.skip && s.title != "" {
				add(s.title)
			}
			break
		}
	}

	// The anchor: the first real paragraph of a section worth reading.
	sec, para := -1, -1
	for i, s := range secs {
		if s.skip {
			continue
		}
		for j, p := range s.paras {
			if wordCount(p) >= minSummaryWords {
				sec, para = i, j
				break
			}
		}
		if sec >= 0 {
			break
		}
	}
	if sec < 0 {
		// Nothing qualifies as a summary; the first paragraphs of whatever
		// is not skipped are still the best available description.
		for i, s := range secs {
			if !s.skip && len(s.paras) > 0 {
				sec, para = i, 0
				break
			}
		}
		if sec < 0 {
			return cutWords(strings.Join(parts, " "), maxWords)
		}
	} else {
		// Short lines between the title and the summary are the tagline —
		// "Fast, small, secure" — and belong with it. Only from the title's
		// own level: a short paragraph under an H2 is a note, not a tagline.
		taglines := 0
		for i := 0; i <= sec && taglines < maxTaglines; i++ {
			s := secs[i]
			if s.skip || s.level > 1 {
				continue
			}
			end := len(s.paras)
			if i == sec {
				end = para
			}
			for _, p := range s.paras[:end] {
				if taglines >= maxTaglines {
					break
				}
				if wordCount(p) < minSummaryWords {
					add(p)
					taglines++
				}
			}
		}
	}

	full := false
	for i := sec; i < len(secs) && !full; i++ {
		s := secs[i]
		if s.skip {
			continue
		}
		start := 0
		if i == sec {
			start = para
		}
		for _, p := range s.paras[start:] {
			if add(p) {
				full = true
				break
			}
		}
	}
	return cutWords(strings.Join(parts, " "), maxWords)
}

// readmeSections strips the markup a README is dressed in and splits what is
// left by heading, keeping the paragraph structure the selection needs.
// Whitespace is collapsed per paragraph, not before: collapsing the whole
// file first is what made the old stripper blind to where a section began.
func readmeSections(markdown string) []section {
	s := strings.ToValidUTF8(markdown, "")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = fenced.ReplaceAllString(s, "\n")
	s = htmlComment.ReplaceAllString(s, "")
	s = refDef.ReplaceAllString(s, "")
	s = mdImage.ReplaceAllString(s, "")
	s = mdLink.ReplaceAllString(s, "$1")
	s = htmlTag.ReplaceAllString(s, " ")
	s = bareURL.ReplaceAllString(s, "")

	var (
		secs []section
		cur  section
		para []string // lines of the paragraph being read
	)
	flush := func() {
		if len(para) == 0 {
			return
		}
		if text := inlineText(strings.Join(para, " ")); text != "" {
			cur.paras = append(cur.paras, text)
		}
		para = nil
	}
	open := func(level int, title string) {
		flush()
		secs = append(secs, cur)
		cur = section{level: level, title: inlineText(title)}
		cur.skip = skipHeading(cur.title)
	}
	prevBlank := true
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
			flush()
			prevBlank = true
			continue
		case prevBlank && (strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "\t")):
			// An indented code block.
			continue
		}
		if m := atxHeading.FindStringSubmatch(line); m != nil {
			open(len(m[1]), m[2])
			prevBlank = true
			continue
		}
		if len(para) > 0 && setextLine.MatchString(line) && !listItem.MatchString(para[len(para)-1]) {
			// The line above was a heading all along.
			level := 2
			if strings.HasPrefix(trimmed, "=") {
				level = 1
			}
			title := para[len(para)-1]
			para = para[:len(para)-1]
			open(level, title)
			prevBlank = true
			continue
		}
		switch {
		case ruleLine.MatchString(line):
			flush()
			prevBlank = true
		case tableRow.MatchString(line), tableDelim.MatchString(line):
			flush()
			prevBlank = false
		case navItem.MatchString(line):
			prevBlank = false
		default:
			para = append(para, trimmed)
			prevBlank = false
		}
	}
	flush()
	return append(secs, cur)
}

// inlineText removes what is left of the markup inside a paragraph —
// emphasis, inline code, blockquote and list markers — and collapses it to
// single spaces.
func inlineText(s string) string {
	s = strings.Map(func(r rune) rune {
		if strings.ContainsRune("#*_`>|-=+~", r) {
			return ' '
		}
		return r
	}, s)
	return strings.TrimSpace(spaces.ReplaceAllString(s, " "))
}

// skipHeading reports whether a section with this heading is one of the ones
// every project has, matched on whole words so that punctuation, emoji and
// numbering around them do not matter.
func skipHeading(title string) bool {
	words := headingWords(title)
	if len(words) == 0 {
		return false
	}
	for _, phrase := range skipHeadings {
		if containsRun(words, phrase) {
			return true
		}
	}
	return false
}

// headingWords lower-cases a heading and splits it into runs of letters and
// digits.
func headingWords(title string) []string {
	return strings.FieldsFunc(strings.ToLower(title), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// containsRun reports whether phrase occurs in words as consecutive elements.
func containsRun(words, phrase []string) bool {
	for i := 0; i+len(phrase) <= len(words); i++ {
		match := true
		for j, w := range phrase {
			if words[i+j] != w {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func wordCount(s string) int {
	return len(strings.Fields(s))
}

// cutWords keeps the first n words of s, collapsed to single spaces; n of
// zero or less keeps them all. Words are whole runes, so nothing is split.
func cutWords(s string, n int) string {
	fields := strings.Fields(s)
	if n > 0 && len(fields) > n {
		fields = fields[:n]
	}
	return strings.Join(fields, " ")
}

// capBytes cuts s to at most limit bytes on a word boundary, or failing that
// on a rune boundary, so that the cut never leaves a partial UTF-8 sequence
// for the tokenizer to read as U+FFFD. A limit of zero or less is no cap.
func capBytes(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	if i := strings.LastIndexByte(s[:cut], ' '); i > 0 {
		cut = i
	}
	return strings.TrimSpace(s[:cut])
}
