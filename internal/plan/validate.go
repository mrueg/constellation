package plan

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/mrueg/constellation/internal/cluster"
)

// Validate checks a plan before anything is written from it.
//
// The plan file exists to be reviewed and edited by hand, and an edit can
// break what Build guarantees: a name GitHub will refuse, a description that
// lost the marker this tool recognises its own lists by, two categories that
// differ only in case and so would silently become one list. Every problem is
// reported in a single error, one per line, so a plan is fixed in one pass
// rather than one refusal at a time.
//
// A category with no repositories is not an error: Build never writes one,
// and Apply and Verify skip one deliberately, so emptying a category is a
// legitimate way of dropping it without deleting the entry.
func (p *Plan) Validate() error {
	var problems []string
	report := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	seenName := map[string]string{} // lowercased -> the name as first written
	for i, cat := range p.Categories {
		label := fmt.Sprintf("category %q", cat.Name)
		if strings.TrimSpace(cat.Name) == "" {
			label = fmt.Sprintf("category #%d", i+1)
			report("%s: the name is empty", label)
		} else if n := utf8.RuneCountInString(cat.Name); n > cluster.MaxListName {
			report("%s: the name is %d characters; GitHub allows %d", label, n, cluster.MaxListName)
		}
		if key := strings.ToLower(strings.TrimSpace(cat.Name)); key != "" {
			if first, dup := seenName[key]; dup {
				report("%s: the name is the same as %q apart from case; GitHub treats them as one list", label, first)
			} else {
				seenName[key] = cat.Name
			}
		}
		if n := utf8.RuneCountInString(cat.Description); n > cluster.MaxListDescription {
			report("%s: the description is %d characters; GitHub allows %d", label, n, cluster.MaxListDescription)
		}
		if !strings.Contains(cat.Description, cluster.DescriptionMarker) {
			report("%s: the description does not carry %q; that is how the tool recognises the lists it made, "+
				"and without it --reconcile and reset will leave this list alone as somebody else's. Restore it",
				label, cluster.DescriptionMarker)
		}
		seenRepo := map[string]bool{}
		for _, r := range cat.Repos {
			if msg := badRepoName(r.FullName); msg != "" {
				report("%s: %s", label, msg)
				continue
			}
			key := strings.ToLower(r.FullName)
			if seenRepo[key] {
				report("%s: %s is listed more than once", label, r.FullName)
				continue
			}
			seenRepo[key] = true
		}
	}
	for _, r := range p.Unassigned {
		if msg := badRepoName(r.FullName); msg != "" {
			report("unassigned: %s", msg)
		}
	}

	if len(problems) == 0 {
		return nil
	}
	return errors.New("the plan has problems that need fixing before it can be applied:\n  " +
		strings.Join(problems, "\n  "))
}

// badRepoName explains what is wrong with a repository name that is not of
// the form owner/name, or returns "" when it is.
func badRepoName(full string) string {
	if strings.TrimSpace(full) == "" {
		return "a repository has no name"
	}
	owner, name, ok := strings.Cut(full, "/")
	switch {
	case !ok || owner == "" || name == "":
		return fmt.Sprintf("%q is not of the form owner/name", full)
	case strings.Contains(name, "/"):
		return fmt.Sprintf("%q has more than one slash; a repository is owner/name", full)
	case strings.ContainsFunc(full, unicode.IsSpace):
		return fmt.Sprintf("%q contains whitespace; a repository is owner/name", full)
	}
	return ""
}
