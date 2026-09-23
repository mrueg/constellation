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
	return VerifyApplied(ctx, c, p, only, nil, out)
}

// VerifyApplied checks what an apply run left the account asserting, which is
// not always the whole plan.
//
// A run that completed asserts every placement in the categories it was asked
// for, less any it skipped because a list of that name was not created here.
// A run that stopped at --limit asserts only the placements it made or found
// in place: holding it to the rest would fail by construction, since it never
// wrote them — which is what made `apply --limit N --verify` always exit 1.
// A nil res verifies the plan as it stands.
func VerifyApplied(ctx context.Context, c *gh.ListsClient, p *Plan, only []string, res *ApplyResult, out io.Writer) (*VerifyResult, error) {
	skip := map[string]bool{}
	if res != nil {
		for _, n := range res.Conflicts {
			skip[strings.ToLower(n)] = true
		}
	}
	var want []placement
	for _, cat := range selected(p.Categories, only) {
		if skip[strings.ToLower(cat.Name)] {
			continue
		}
		var repos []string
		if res != nil && res.StoppedAtCap {
			repos = res.Placed[cat.Name]
		} else {
			for _, r := range cat.Repos {
				repos = append(repos, r.FullName)
			}
		}
		if len(repos) == 0 {
			continue
		}
		want = append(want, placement{list: cat.Name, repos: repos})
	}
	return verifyPlacements(ctx, c, want, out)
}

// placement is one list and the repositories that must be in it.
type placement struct {
	list  string
	repos []string
}

func verifyPlacements(ctx context.Context, c *gh.ListsClient, want []placement, out io.Writer) (*VerifyResult, error) {
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

	for _, pl := range want {
		name := strings.ToLower(pl.list)
		if _, ok := byName[name]; !ok {
			res.MissingLists = append(res.MissingLists, pl.list)
			continue
		}
		for _, r := range pl.repos {
			res.Checked++
			if !filed[r][name] {
				res.Missing[pl.list] = append(res.Missing[pl.list], r)
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
