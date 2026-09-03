package plan

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/mrueg/constellation/internal/cluster"
	"github.com/mrueg/constellation/internal/gh"
	"github.com/mrueg/constellation/internal/ui"
)

// ApplyOptions control how a plan is written back to GitHub.
type ApplyOptions struct {
	// Delay paces the writes. GitHub's documented ceiling for mutating
	// requests is 500 an hour, and its own advice is at least a second
	// between them; going faster earns escalating secondary rate limits
	// rather than a refusal.
	Delay time.Duration
	// Limit stops after this many repository writes; 0 means no limit. It
	// counts removals as well as additions, and reconciling is refused
	// alongside it — otherwise a "cautious first run" could delete every list
	// the plan has dropped before the limit was ever consulted.
	Limit int
	// Only restricts the apply to the named categories.
	Only []string
	// ContinueOnError keeps going past a failed repository instead of aborting.
	ContinueOnError bool
	// DryRun reports what would change without changing anything.
	DryRun bool
	// Reconcile makes the account match the plan instead of only adding to it:
	// repositories leave categories they no longer belong to, and categories
	// the plan has dropped are deleted.
	//
	// Only lists this tool wrote are touched, recognized by the marker their
	// descriptions carry. A list made by hand is left exactly alone.
	Reconcile bool
	// Private creates and keeps the lists private. Visibility is enforced on
	// every run, not only at creation.
	Private bool
	// Adopt lets apply take over an existing list whose name matches a planned
	// category but which this tool did not create.
	//
	// Taking one over replaces its description with the plan's, and that
	// description carries the marker identifying a list as this tool's — so an
	// adopted list becomes subject to --reconcile and to reset. It is
	// announced when it happens, because neither consequence is recoverable
	// from the flag's name.
	Adopt bool
	// Batch sends this many membership changes in one GraphQL request. One is
	// the conservative default: GraphQL executes batched mutations serially so
	// the effect is identical, but whether GitHub meters a batch as one
	// mutating request or as many is not documented, and that is what decides
	// how long a large apply takes.
	Batch int
	// Force makes room when the plan does not fit inside GitHub's cap by
	// deleting lists this tool created that the plan no longer contains. Lists
	// made by hand are never deleted, and neither is a list the plan still
	// has — so this only removes what a --reconcile run would have removed
	// anyway, and only as many as are needed to fit.
	Force bool
	Out   io.Writer
}

// ApplyResult reports what actually happened.
type ApplyResult struct {
	ListsDeleted []string
	ReposRemoved int
	ListsCreated []string
	ReposFiled   int
	ReposSkipped int
	Errors       []error
	StoppedAtCap bool
	// Conflicts are existing lists left untouched because their name matches a
	// planned category but they were not created here.
	Conflicts []string
	// Adopted are lists that were taken over with --adopt: their descriptions
	// were replaced and they are now managed here.
	Adopted []string
}

// Apply creates the planned lists and files each repository into its category.
//
// The whole account is read once, so every decision below is made against
// known state rather than by asking GitHub about each repository in turn. A
// repository is written at most once: its complete set of memberships is
// computed first — additions from the plan, removals from reconciling — and
// submitted in a single mutation, because updateUserListsForItem replaces the
// whole set. Membership the plan knows nothing about is carried across
// untouched, which is what stops an apply emptying a hand-curated list.
func Apply(ctx context.Context, c *gh.ListsClient, p *Plan, opt ApplyOptions) (*ApplyResult, error) {
	out := opt.Out
	if out == nil {
		out = io.Discard
	}
	res := &ApplyResult{}

	existing, err := c.Lists(ctx)
	if err != nil {
		return nil, err
	}
	// Reading contents is the slow part, so the planned lists are read first:
	// they alone determine whether there is anything to do, and a re-run that
	// finds nothing to do can stop there.
	//
	// Anything that does need writing then forces the rest to be read as well,
	// because updateUserListsForItem replaces a repository's whole set of
	// lists. Writing one without knowing the hand-curated lists it is already
	// in would silently remove it from them.
	// Scoped to the categories this run will actually touch, so --only stays
	// cheap; the full read below still happens before anything is written.
	firstPass := map[string]bool{}
	for _, cat := range selected(p.Categories, opt.Only) {
		firstPass[strings.ToLower(cat.Name)] = true
	}
	read := map[string]bool{}
	readContents := func(want func(gh.List) bool) error {
		for i := range existing {
			if existing[i].Count == 0 || read[existing[i].ID] || !want(existing[i]) {
				continue
			}
			repos, err := c.ListItems(ctx, existing[i].ID)
			if err != nil {
				return fmt.Errorf("reading the contents of %q: %w", existing[i].Name, err)
			}
			existing[i].Repos = repos
			read[existing[i].ID] = true
		}
		return nil
	}
	if err := readContents(func(l gh.List) bool { return firstPass[strings.ToLower(l.Name)] }); err != nil {
		return nil, err
	}

	byName := map[string]gh.List{}
	ours := map[string]bool{}
	for _, l := range existing {
		byName[strings.ToLower(l.Name)] = l
		// The marker identifies a list this tool wrote; anything else is the
		// user's and is never adopted, renamed or deleted without --adopt.
		if strings.Contains(l.Description, cluster.DescriptionMarker) {
			ours[strings.ToLower(l.Name)] = true
		}
	}

	// current[repo] is the set of list ids the repository is in right now.
	current := map[string]map[string]bool{}
	for _, l := range existing {
		for _, r := range l.Repos {
			if current[r] == nil {
				current[r] = map[string]bool{}
			}
			current[r][l.ID] = true
		}
	}

	cats := selected(p.Categories, opt.Only)
	if len(cats) == 0 {
		return res, fmt.Errorf("no categories selected")
	}

	// Categories whose name collides with a list this tool did not create are
	// dropped before anything is written, so a hand-curated list is never
	// described, filed into, or marked.
	var work []Category
	for _, cat := range cats {
		name := strings.ToLower(cat.Name)
		if len(cat.Repos) == 0 {
			continue
		}
		if _, exists := byName[name]; exists && !ours[name] {
			if !opt.Adopt {
				fmt.Fprintf(out, "%s %s: a list with this name already exists and was not created here; --adopt to take it over\n",
					ui.Warn("skipping"), ui.Name(cat.Name))
				res.Conflicts = append(res.Conflicts, cat.Name)
				continue
			}
			fmt.Fprintf(out, "%s %s: its description will be replaced, and it will from now on be managed here — so --reconcile and reset can delete it\n",
				ui.Warn("adopting"), ui.Name(cat.Name))
			res.Adopted = append(res.Adopted, cat.Name)
			// Adopting makes the list ours for the rest of this run, or
			// reconcile would announce that it is leaving alone the very list
			// about to be taken over.
			ours[name] = true
		}
		work = append(work, cat)
	}
	// Reconciling deletes lists before the write loop begins, so no limit on
	// that loop can bound it. Refuse rather than let --limit read as a safety
	// belt it is not.
	if opt.Limit > 0 && (opt.Reconcile || opt.Force) {
		return res, fmt.Errorf("--limit cannot bound --reconcile or --force: both delete lists " +
			"before any repository is written. Preview with --dry-run, or drop --limit")
	}
	// Making room means deleting lists the plan has dropped, and those are by
	// definition not ones --only can name. Reconciling has a narrower meaning
	// available and takes it; freeing space does not.
	if opt.Force && len(opt.Only) > 0 {
		return res, fmt.Errorf("--force cannot be combined with --only: making room means deleting " +
			"lists this run was not asked to touch. Run without --only, or free space first")
	}

	planned := map[string]bool{}
	for _, cat := range p.Categories {
		planned[strings.ToLower(cat.Name)] = true
	}

	// Non-nil only when the run was narrowed with --only.
	var within map[string]bool
	if len(opt.Only) > 0 {
		within = map[string]bool{}
		for _, cat := range selected(p.Categories, opt.Only) {
			within[strings.ToLower(cat.Name)] = true
		}
	}

	if opt.Reconcile {
		// Removals are decided from every list this tool manages, so they all
		// have to be read before reconciling.
		if err := readContents(func(l gh.List) bool {
			name := strings.ToLower(l.Name)
			return ours[name] && (within == nil || within[name])
		}); err != nil {
			return res, err
		}
		kept, err := reconcile(ctx, c, existing, planned, ours, within, opt, res, out)
		if err != nil {
			return res, err
		}
		// The surviving set is used in a dry run too. Nothing is deleted there,
		// but every decision after this point — above all whether the plan
		// fits in GitHub's cap — is about the account as it would be once the
		// run finished. Keeping the pre-reconcile picture made a dry run
		// report deletions and then refuse for lack of room those very
		// deletions would have freed.
		existing = kept
		byName = map[string]gh.List{}
		for _, l := range existing {
			byName[strings.ToLower(l.Name)] = l
		}
	}

	// The budget is checked after reconciling, because reconciling deletes the
	// categories the plan has dropped and that is exactly what frees room.
	if err := checkListBudget(work, existing, byName); err != nil {
		if !opt.Force {
			return res, err
		}
		freed, ferr := freeRoom(ctx, c, existing, byName, planned, ours, work, opt, res, out)
		if ferr != nil {
			return res, ferr
		}
		existing = freed
		byName = map[string]gh.List{}
		for _, l := range existing {
			byName[strings.ToLower(l.Name)] = l
		}
		if err := checkListBudget(work, existing, byName); err != nil {
			return res, err
		}
	}

	// Bring every planned list into existence and into line with the plan.
	wantList := map[string]string{} // lowercased category name -> list id
	for _, cat := range work {
		name := strings.ToLower(cat.Name)
		if l, exists := byName[name]; exists {
			wantList[name] = l.ID
			if opt.DryRun {
				fmt.Fprintf(out, "would describe list %s and make it %s\n", ui.Name(cat.Name), visibility(opt.Private))
				continue
			}
			if err := c.UpdateList(ctx, l.ID, cat.Name, cat.Description, opt.Private); err != nil {
				if !opt.ContinueOnError || terminal(ctx, err) {
					return res, err
				}
				res.Errors = append(res.Errors, err)
				continue
			}
			fmt.Fprintf(out, "described list %s (%s)\n", ui.Name(cat.Name), visibility(opt.Private))
			pause(ctx, opt.Delay)
			continue
		}
		if opt.DryRun {
			fmt.Fprintf(out, "would create %s list %s\n", visibility(opt.Private), ui.Name(cat.Name))
			res.ListsCreated = append(res.ListsCreated, cat.Name)
			continue
		}
		l, err := c.CreateList(ctx, cat.Name, cat.Description, opt.Private)
		if err != nil {
			if !opt.ContinueOnError || terminal(ctx, err) {
				return res, err
			}
			res.Errors = append(res.Errors, err)
			continue
		}
		wantList[name] = l.ID
		byName[name] = l
		ours[name] = true
		res.ListsCreated = append(res.ListsCreated, cat.Name)
		fmt.Fprintf(out, "%s %s list %s\n", ui.Added("created"), visibility(opt.Private), ui.Name(cat.Name))
		pause(ctx, opt.Delay)
	}

	// Removals are only considered when reconciling, and are merged into the
	// same per-repository write as the additions so nothing is touched twice.
	stale := map[string][]string{}
	if opt.Reconcile {
		stale = StalePlacements(existing, p, ours, within)
	}

	// Work out what each repository needs. One already in its category and
	// with nothing to lose needs no request at all, which is what makes a
	// re-run cheap.
	type change struct {
		repo string
		add  []string
		drop []string
	}
	var changes []change
	seen := map[string]int{} // repo -> index into changes
	for _, cat := range work {
		listID, ok := wantList[strings.ToLower(cat.Name)]
		if !ok && !opt.DryRun {
			continue // creation failed and --continue is on
		}
		for _, r := range cat.Repos {
			if current[r.FullName][listID] {
				res.ReposSkipped++
				continue
			}
			if i, ok := seen[r.FullName]; ok {
				changes[i].add = append(changes[i].add, listID)
				continue
			}
			seen[r.FullName] = len(changes)
			changes = append(changes, change{repo: r.FullName, add: []string{listID}})
		}
	}

	for _, repo := range sortedRepoNames(stale) {
		if i, ok := seen[repo]; ok {
			changes[i].drop = stale[repo]
			continue
		}
		seen[repo] = len(changes)
		changes = append(changes, change{repo: repo, drop: stale[repo]})
	}

	prog := &progress{total: len(changes), started: time.Now(), out: out}
	if len(changes) == 0 {
		return res, nil
	}

	// Something is going to be written, so every remaining list has to be read
	// now: the mutation replaces a repository's whole set of memberships, and
	// one that is not known here would be dropped.
	if !opt.DryRun {
		if err := readContents(func(gh.List) bool { return true }); err != nil {
			return res, err
		}
		current = map[string]map[string]bool{}
		for _, l := range existing {
			for _, r := range l.Repos {
				if current[r] == nil {
					current[r] = map[string]bool{}
				}
				current[r][l.ID] = true
			}
		}
	}

	// Node ids are needed to write, and only for repositories that are
	// actually changing; they are resolved a hundred at a time.
	ids := map[string]string{}
	if !opt.DryRun {
		names := make([]string, 0, len(changes))
		for _, ch := range changes {
			names = append(names, ch.repo)
		}
		if ids, err = c.RepoIDs(ctx, names); err != nil {
			return res, err
		}
	}

	batchSize := max(opt.Batch, 1)
	var pending []gh.ItemLists
	var applied []change

	// flush writes the accumulated changes and folds the outcome back into the
	// result. Failures are per repository even inside a batch, because GitHub
	// answers with the successes intact and names the field that failed.
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		failed, err := c.SetItemListsBatch(ctx, pending)
		if err != nil {
			if !opt.ContinueOnError || terminal(ctx, err) {
				return fmt.Errorf("filing %d repositories: %w", len(pending), err)
			}
			res.Errors = append(res.Errors, err)
			pending, applied = nil, nil
			return nil
		}
		for i, it := range pending {
			ch := applied[i]
			if e, bad := failed[it.Name]; bad {
				res.Errors = append(res.Errors, fmt.Errorf("filing %s: %w", it.Name, e))
				fmt.Fprintf(out, "  %s %s: %v\n", ui.Removed("!"), ui.Name(it.Name), e)
				continue
			}
			set := map[string]bool{}
			for _, lid := range it.ListIDs {
				set[lid] = true
			}
			current[it.Name] = set
			res.ReposRemoved += len(ch.drop)
			if len(ch.add) > 0 {
				res.ReposFiled++
			}
			fmt.Fprintf(out, "%s %s %s\n", ui.Muted(prog.label()), ui.Added("+"), ui.Name(it.Name))
		}
		pending, applied = nil, nil
		pause(ctx, opt.Delay)
		return nil
	}

	for _, ch := range changes {
		if opt.Limit > 0 && res.ReposFiled+res.ReposRemoved >= opt.Limit {
			if err := flush(); err != nil {
				return res, err
			}
			res.StoppedAtCap = true
			return res, nil
		}
		prog.step()
		if opt.DryRun {
			switch {
			case len(ch.add) > 0 && len(ch.drop) > 0:
				fmt.Fprintf(out, "%s would file %s and remove it from %d\n",
					ui.Muted(prog.label()), ui.Name(ch.repo), len(ch.drop))
			case len(ch.drop) > 0:
				fmt.Fprintf(out, "%s would remove %s from %d\n",
					ui.Muted(prog.label()), ui.Name(ch.repo), len(ch.drop))
			default:
				fmt.Fprintf(out, "%s would file %s\n", ui.Muted(prog.label()), ui.Name(ch.repo))
			}
			res.ReposRemoved += len(ch.drop)
			if len(ch.add) > 0 {
				res.ReposFiled++
			}
			continue
		}
		id, ok := ids[ch.repo]
		if !ok {
			err := fmt.Errorf("filing %s: GitHub does not know this repository (renamed or deleted?)", ch.repo)
			if !opt.ContinueOnError {
				return res, err
			}
			res.Errors = append(res.Errors, err)
			fmt.Fprintf(out, "  %s %s\n", ui.Removed("!"), ui.Name(ch.repo))
			continue
		}
		// Everything it is already in, plus everything being added: the
		// mutation replaces the set, so anything omitted here is removed.
		want := map[string]bool{}
		for lid := range current[ch.repo] {
			want[lid] = true
		}
		for _, lid := range ch.drop {
			delete(want, lid)
		}
		for _, lid := range ch.add {
			want[lid] = true
		}
		pending = append(pending, gh.ItemLists{RepoID: id, ListIDs: sortedKeysOf(want), Name: ch.repo})
		applied = append(applied, ch)
		if len(pending) < batchSize {
			continue
		}
		if err := flush(); err != nil {
			return res, err
		}
	}
	if err := flush(); err != nil {
		return res, err
	}
	return res, nil
}

// reconcile removes what the plan no longer asserts: categories it has dropped,
// and repositories that have left one.
// reconcile removes what the plan no longer asserts and returns the lists that
// survive, so the caller's picture of the account stays accurate — the budget
// check that follows depends on it.
func reconcile(ctx context.Context, c *gh.ListsClient, existing []gh.List,
	planned map[string]bool, ours map[string]bool, within map[string]bool,
	opt ApplyOptions, res *ApplyResult, out io.Writer) ([]gh.List, error) {

	// A category the plan has dropped is, by definition, not one --only can
	// name — so a narrowed run cannot be asked to delete it, and deleting it
	// anyway would make --only mean something far wider than it says. Under
	// --only, reconciling brings the named categories in line and deletes
	// nothing.
	if within != nil {
		fmt.Fprintf(out, "%s reconciling only the categories named; no lists will be deleted\n",
			ui.Info("scoped:"))
	}

	// Deleting a list removes every placement in it, so dropped categories are
	// handled first and their members need no per-repository write.
	var kept []gh.List
	for _, l := range existing {
		name := strings.ToLower(l.Name)
		if !ours[name] {
			fmt.Fprintf(out, "%s %s alone; it was not created here\n", ui.Info("leaving"), ui.Name(l.Name))
			kept = append(kept, l)
			continue
		}
		if planned[name] || within != nil {
			kept = append(kept, l)
			continue
		}
		if opt.DryRun {
			fmt.Fprintf(out, "would delete list %s; the plan no longer has it\n", ui.Name(l.Name))
			res.ListsDeleted = append(res.ListsDeleted, l.Name)
			continue
		}
		if err := c.DeleteList(ctx, l.ID); err != nil {
			if !opt.ContinueOnError || terminal(ctx, err) {
				return nil, fmt.Errorf("deleting list %q: %w", l.Name, err)
			}
			res.Errors = append(res.Errors, err)
			kept = append(kept, l)
			continue
		}
		res.ListsDeleted = append(res.ListsDeleted, l.Name)
		fmt.Fprintf(out, "%s list %s; the plan no longer has it\n", ui.Removed("deleted"), ui.Name(l.Name))
		pause(ctx, opt.Delay)
	}
	return kept, nil
}

// StalePlacements reports repositories that are in a managed list the plan no
// longer puts them in. It is separate from Apply's additions because removals
// are the destructive half and are only performed when reconciling.
// within, when non-nil, restricts which lists may be stripped. Membership is
// still judged against the whole plan — a repository leaves a list only when
// the plan does not put it there — but the lists considered are limited to the
// ones the run was asked to touch.
func StalePlacements(existing []gh.List, p *Plan, ours map[string]bool, within map[string]bool) map[string][]string {
	wanted := map[string]map[string]bool{}
	for _, c := range p.Categories {
		for _, r := range c.Repos {
			if wanted[r.FullName] == nil {
				wanted[r.FullName] = map[string]bool{}
			}
			wanted[r.FullName][strings.ToLower(c.Name)] = true
		}
	}
	stale := map[string][]string{}
	for _, l := range existing {
		name := strings.ToLower(l.Name)
		if !ours[name] || (within != nil && !within[name]) {
			continue
		}
		for _, r := range l.Repos {
			if !wanted[r][strings.ToLower(l.Name)] {
				stale[r] = append(stale[r], l.ID)
			}
		}
	}
	return stale
}

func sortedKeysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func selected(cats []Category, only []string) []Category {
	if len(only) == 0 {
		return cats
	}
	want := map[string]bool{}
	for _, o := range only {
		want[strings.ToLower(strings.TrimSpace(o))] = true
	}
	var out []Category
	for _, c := range cats {
		if want[strings.ToLower(c.Name)] {
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i].Repos) > len(out[j].Repos) })
	return out
}

func pause(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	select {
	case <-time.After(d):
	case <-ctx.Done():
	}
}

// checkListBudget refuses to start when the plan cannot fit inside GitHub's
// cap on star lists, rather than discovering it partway through a long run.
func checkListBudget(cats []Category, existing []gh.List, byName map[string]gh.List) error {
	needed := 0
	for _, c := range cats {
		if _, exists := byName[strings.ToLower(c.Name)]; len(c.Repos) > 0 && !exists {
			needed++
		}
	}
	total := len(existing) + needed
	if total <= gh.MaxLists {
		return nil
	}
	room := max(gh.MaxLists-len(existing), 0)
	return fmt.Errorf("GitHub allows %d star lists per account; you already have %d and this plan needs %d more (%d in total). "+
		"Re-run `constellation plan --max-clusters %d` to group your stars into what is left, or apply a subset with --only",
		gh.MaxLists, len(existing), needed, total, room)
}

type progress struct {
	total, done int
	started     time.Time
	out         io.Writer
}

func (p *progress) step() { p.done++ }

// roundETA keeps the estimate readable: whole minutes once there is more than a
// minute to go, whole seconds below that.
func roundETA(d time.Duration) time.Duration {
	if d >= time.Minute {
		return d.Round(time.Minute)
	}
	if d < time.Second {
		return time.Second
	}
	return d.Round(time.Second)
}

// label renders the counter and, once there is enough history to mean
// anything, how long the rest is likely to take.
func (p *progress) label() string {
	pct := 0
	if p.total > 0 {
		pct = 100 * p.done / p.total
	}
	if p.done < 20 || p.done >= p.total {
		return fmt.Sprintf("[%d/%d %d%%]", p.done, p.total, pct)
	}
	per := time.Since(p.started) / time.Duration(p.done)
	left := time.Duration(p.total-p.done) * per
	return fmt.Sprintf("[%d/%d %d%% ~%s left]", p.done, p.total, pct, roundETA(left))
}

func visibility(private bool) string {
	if private {
		return "private"
	}
	return "public"
}

// terminal reports whether the run is over for a reason that will not change by
// moving to the next item: it was interrupted, or the token cannot write.
func terminal(ctx context.Context, err error) bool {
	return ctx.Err() != nil ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, gh.ErrNeedsUserScope)
}

func sortedRepoNames(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// freeRoom deletes lists this tool created that the plan no longer contains,
// stopping as soon as the plan fits. It never touches a list made by hand, nor
// one the plan still has; the worst it can do is remove a category an earlier
// run created and this plan has dropped.
//
// Lists are taken smallest-first, so the most work survives if the run is
// interrupted partway.
func freeRoom(ctx context.Context, c *gh.ListsClient, existing []gh.List, byName map[string]gh.List,
	planned, ours map[string]bool, work []Category, opt ApplyOptions, res *ApplyResult, out io.Writer) ([]gh.List, error) {

	var deletable []gh.List
	for _, l := range existing {
		name := strings.ToLower(l.Name)
		if ours[name] && !planned[name] {
			deletable = append(deletable, l)
		}
	}
	sort.SliceStable(deletable, func(i, j int) bool { return deletable[i].Count < deletable[j].Count })

	keep := existing
	for _, l := range deletable {
		if checkListBudget(work, keep, byName) == nil {
			break
		}
		if opt.DryRun {
			fmt.Fprintf(out, "would delete list %s to make room\n", ui.Name(l.Name))
		} else {
			if err := c.DeleteList(ctx, l.ID); err != nil {
				return nil, fmt.Errorf("deleting list %q to make room: %w", l.Name, err)
			}
			fmt.Fprintf(out, "%s list %s to make room\n", ui.Removed("deleted"), ui.Name(l.Name))
			pause(ctx, opt.Delay)
		}
		res.ListsDeleted = append(res.ListsDeleted, l.Name)
		delete(byName, strings.ToLower(l.Name))
		var next []gh.List
		for _, x := range keep {
			if x.ID != l.ID {
				next = append(next, x)
			}
		}
		keep = next
	}
	return keep, nil
}
