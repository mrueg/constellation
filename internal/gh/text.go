package gh

import (
	"regexp"
	"strings"
)

var (
	fenced   = regexp.MustCompile("(?s)```.*?```")
	htmlTag  = regexp.MustCompile(`(?s)<[^>]+>`)
	mdLink   = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	badgeImg = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)`)
	spaces   = regexp.MustCompile(`\s+`)
)

// stripMarkdown reduces a README to prose. Badges, code blocks and link URLs
// are pure noise for categorization — a shields.io URL appears in thousands of
// unrelated repositories and would cluster them together.
func stripMarkdown(s string) string {
	s = fenced.ReplaceAllString(s, " ")
	s = badgeImg.ReplaceAllString(s, " ")
	s = mdLink.ReplaceAllString(s, "$1")
	s = htmlTag.ReplaceAllString(s, " ")
	s = strings.Map(func(r rune) rune {
		if strings.ContainsRune("#*_`>|-=+", r) {
			return ' '
		}
		return r
	}, s)
	return strings.TrimSpace(spaces.ReplaceAllString(s, " "))
}
