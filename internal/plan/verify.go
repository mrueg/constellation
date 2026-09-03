package plan

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/mrueg/constellation/internal/gh"
	"github.com/mrueg/constellation/internal/ui"
)

// VerifyResult reports the difference between what the plan asserts and what
// the account actually holds.
type VerifyResult struct {
	// Missing maps a category to the repositories the plan places there that
	// the account does not have.
	Missing map[string][]string
	// MissingLists are planned categories with no list at all.
	MissingLists []string
	Checked      int
}

// OK reports whether the account matches the plan.
func (v *VerifyResult) OK() bool {
	return len(v.Missing) == 0 && len(v.MissingLists) == 0
}

// Verify re-reads the account and checks that every placement the plan asserts
// is really there.
//
// This exists because a mutation reporting success is not proof the placement
// is visible. updateUserListsForItem replaces a repository's whole set of
// lists, so a partial write, or one racing another client, would not surface
// as an error. Reading the result back is the only real confirmation.
//
// It costs one page read per list, not one per repository, so it is cheap
// enough to run after every apply.
func Verify(ctx context.Context, c *gh.ListsClient, p *Plan, only []string, out io.Writer) (*VerifyResult, error) {
	if out == nil {
		out = io.Discard
	}
	res := &VerifyResult{Missing: map[string][]string{}}

	existing, err := c.ListsWithRepos(ctx)
	if err != nil {
		return nil, err
	}
	byName := map[string]gh.List{}
	filed := map[string]map[string]bool{}
	for _, l := range existing {
		name := strings.ToLower(l.Name)
		byName[name] = l
		for _, r := range l.Repos {
			if filed[r] == nil {
				filed[r] = map[string]bool{}
			}
			filed[r][name] = true
		}
	}

	for _, cat := range selected(p.Categories, only) {
		if len(cat.Repos) == 0 {
			continue
		}
		name := strings.ToLower(cat.Name)
		if _, ok := byName[name]; !ok {
			res.MissingLists = append(res.MissingLists, cat.Name)
			continue
		}
		for _, r := range cat.Repos {
			res.Checked++
			if !filed[r.FullName][name] {
				res.Missing[cat.Name] = append(res.Missing[cat.Name], r.FullName)
			}
		}
	}

	for _, n := range res.MissingLists {
		fmt.Fprintf(out, "%s list %s\n", ui.Warn("missing"), ui.Name(n))
	}
	for _, name := range sortedKeys(res.Missing) {
		fmt.Fprintf(out, "%s: %s\n", ui.Name(name),
			ui.Warn(fmt.Sprintf("%d repositories are not in the list", len(res.Missing[name]))))
	}
	return res, nil
}

func sortedKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
