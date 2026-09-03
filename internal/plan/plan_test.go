package plan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mrueg/constellation/internal/cluster"
	"github.com/mrueg/constellation/internal/gh"
)

// fakeGitHub is a minimal stand-in for GitHub's GraphQL API: it answers the
// list queries and mutations the tool issues and remembers what was written.
type fakeGitHub struct {
	mu        sync.Mutex
	lists     []string          // ordered names
	listID    map[string]string // name -> stable id, as GitHub's node ids are
	nextID    int
	described map[string]string   // list name -> description
	private   map[string]bool     // list name -> visibility
	member    map[string][]string // repo -> list ids
	posts     int                 // mutations received
	listReads int                 // list queries served
	itemReads map[string]int      // list id -> content pages served
	failWith  string              // when set, every request answers with this GraphQL error type
}

// id assigns a stable identifier the first time a list is named, so deleting
// one never renumbers the others — mirroring GitHub's node ids. Getting this
// wrong in the fake made reset appear to delete the wrong list.
func (f *fakeGitHub) id(name string) string {
	if id, ok := f.listID[name]; ok {
		return id
	}
	f.nextID++
	id := fmt.Sprintf("L%d", f.nextID)
	f.listID[name] = id
	return id
}

func (f *fakeGitHub) nameOf(id string) string {
	for name, got := range f.listID {
		if got == id {
			return name
		}
	}
	return ""
}

func (f *fakeGitHub) in(repo, list string) bool {
	for _, id := range f.member[repo] {
		if id == f.id(list) {
			return true
		}
	}
	return false
}

func (f *fakeGitHub) reposIn(id string) []string {
	var out []string
	for repo, ids := range f.member {
		for _, got := range ids {
			if got == id {
				out = append(out, repo)
			}
		}
	}
	sort.Strings(out) // deterministic output for the tests
	return out
}

// slug mirrors how GitHub derives a list's slug from its name.
func slugOf(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimRight(b.String(), "-")
}

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	q, v := req.Query, req.Variables

	if f.failWith != "" {
		fmt.Fprintf(w, `{"errors":[{"type":%q,"message":"denied"}]}`, f.failWith)
		return
	}

	str := func(k string) string { s, _ := v[k].(string); return s }
	boolean := func(k string) bool { b, _ := v[k].(bool); return b }

	switch {
	case strings.Contains(q, "viewer { login }"):
		fmt.Fprint(w, `{"data":{"viewer":{"login":"octocat"}}}`)

	case strings.Contains(q, "createUserList"):
		f.posts++
		name := str("name")
		f.lists = append(f.lists, name)
		f.described[name] = str("description")
		f.private[name] = boolean("isPrivate")
		fmt.Fprintf(w, `{"data":{"createUserList":{"list":{"id":%q,"name":%q,"slug":%q}}}}`,
			f.id(name), name, slugOf(name))

	case strings.Contains(q, "updateUserList("):
		f.posts++
		if name := f.nameOf(str("listId")); name != "" {
			f.described[name] = str("description")
			f.private[name] = boolean("isPrivate")
		}
		fmt.Fprint(w, `{"data":{"updateUserList":{"list":{"id":"1"}}}}`)

	case strings.Contains(q, "deleteUserList"):
		f.posts++
		id := str("listId")
		name := f.nameOf(id)
		var kept []string
		for _, l := range f.lists {
			if l != name {
				kept = append(kept, l)
			}
		}
		// Deleting a list drops every membership in it.
		for repo, ids := range f.member {
			var next []string
			for _, got := range ids {
				if got != id {
					next = append(next, got)
				}
			}
			f.member[repo] = next
		}
		delete(f.listID, name)
		f.lists = kept
		fmt.Fprint(w, `{"data":{"deleteUserList":{"user":{"id":"u"}}}}`)

	case strings.Contains(q, "updateUserListsForItem"):
		// One request may carry several aliased mutations; GraphQL runs them
		// serially, so applying them in index order matches GitHub.
		f.posts++
		var b strings.Builder
		b.WriteString(`{"data":{`)
		for i := 0; ; i++ {
			item, ok := v[fmt.Sprintf("i%d", i)].(string)
			if !ok {
				break
			}
			var ids []string
			if raw, ok := v[fmt.Sprintf("l%d", i)].([]any); ok {
				for _, x := range raw {
					if id, ok := x.(string); ok {
						ids = append(ids, id)
					}
				}
			}
			f.member[strings.TrimPrefix(item, "id:")] = ids
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `"m%d":{"item":{"__typename":"Repository"}}`, i)
		}
		b.WriteString(`}}`)
		fmt.Fprint(w, b.String())

	case strings.Contains(q, "repository(owner:"):
		// RepoIDs resolves names to node ids with one aliased field each.
		var b strings.Builder
		b.WriteString(`{"data":{`)
		first := true
		for _, m := range regexp.MustCompile(`(r\d+): repository\(owner: "([^"]*)", name: "([^"]*)"\)`).FindAllStringSubmatch(q, -1) {
			if !first {
				b.WriteString(",")
			}
			first = false
			full := m[2] + "/" + m[3]
			fmt.Fprintf(&b, `%q:{"id":"id:%s","nameWithOwner":%q}`, m[1], full, full)
		}
		b.WriteString(`}}`)
		fmt.Fprint(w, b.String())

	case strings.Contains(q, "node(id:"):
		if f.itemReads == nil {
			f.itemReads = map[string]int{}
		}
		f.itemReads[str("id")]++
		var items strings.Builder
		for j, r := range f.reposIn(str("id")) {
			if j > 0 {
				items.WriteString(",")
			}
			fmt.Fprintf(&items, `{"__typename":"Repository","nameWithOwner":%q}`, r)
		}
		fmt.Fprintf(w, `{"data":{"node":{"items":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[%s]}}}}`,
			items.String())

	case strings.Contains(q, "lists(first:"):
		f.listReads++
		var b strings.Builder
		b.WriteString(`{"data":{"viewer":{"lists":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[`)
		for i, name := range f.lists {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `{"id":%q,"name":%q,"slug":%q,"description":%q,"isPrivate":%v,`+
				`"items":{"totalCount":%d}}`,
				f.id(name), name, slugOf(name), f.described[name], f.private[name],
				len(f.reposIn(f.id(name))))
		}
		b.WriteString(`]}}}}`)
		fmt.Fprint(w, b.String())

	default:
		fmt.Fprint(w, `{"data":{}}`)
	}
}

func testSetup(t *testing.T) (*fakeGitHub, *gh.ListsClient, *Plan) {
	t.Helper()
	f := &fakeGitHub{member: map[string][]string{}, described: map[string]string{},
		private: map[string]bool{}, listID: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	c, err := gh.NewListsClient("test-token", gh.WithHTTPClient(srv.Client()), gh.WithEndpoint(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	p := &Plan{
		Version: version, User: "octocat", Backend: "tfidf", TotalStars: 4,
		Categories: []Category{
			// Descriptions carry the marker, as every description this tool
			// writes does; without it a second run would treat its own lists
			// as somebody else's and refuse to touch them.
			{Name: "Kubernetes", Description: "k8s things. " + cluster.DescriptionMarker, Repos: []Repo{
				{FullName: "helm/helm"}, {FullName: "k3s-io/k3s"},
			}},
			{Name: "Rust", Description: "rust things. " + cluster.DescriptionMarker, Repos: []Repo{
				{FullName: "tokio-rs/tokio"},
			}},
		},
	}
	return f, c, p
}

func TestApplyCreatesListsAndFilesRepos(t *testing.T) {
	f, c, p := testSetup(t)
	res, err := Apply(context.Background(), c, p, ApplyOptions{Out: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ListsCreated) != 2 {
		t.Errorf("created %v, want both lists", res.ListsCreated)
	}
	if res.ReposFiled != 3 {
		t.Errorf("filed %d repositories, want 3", res.ReposFiled)
	}
	for _, tc := range []struct{ repo, list string }{
		{"helm/helm", "Kubernetes"}, {"k3s-io/k3s", "Kubernetes"}, {"tokio-rs/tokio", "Rust"},
	} {
		if !f.in(tc.repo, tc.list) {
			t.Errorf("%s was not filed under %s", tc.repo, tc.list)
		}
	}
}

// Re-running must be a no-op. An interrupted apply is the normal case, and it
// has to be safe to just run again.
func TestApplyIsIdempotent(t *testing.T) {
	f, c, p := testSetup(t)
	if _, err := Apply(context.Background(), c, p, ApplyOptions{Out: io.Discard}); err != nil {
		t.Fatal(err)
	}
	res, err := Apply(context.Background(), c, p, ApplyOptions{Out: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ListsCreated) != 0 {
		t.Errorf("re-running created %v, want nothing", res.ListsCreated)
	}
	if res.ReposFiled != 0 || res.ReposSkipped != 3 {
		t.Errorf("re-running filed %d and skipped %d, want 0 and 3", res.ReposFiled, res.ReposSkipped)
	}
	if len(f.lists) != 2 {
		t.Errorf("re-running left %d lists, want 2", len(f.lists))
	}
}

func TestApplyRespectsLimitAndOnly(t *testing.T) {
	_, c, p := testSetup(t)
	res, err := Apply(context.Background(), c, p, ApplyOptions{Limit: 1, Out: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if res.ReposFiled != 1 || !res.StoppedAtCap {
		t.Errorf("limit ignored: filed %d, stopped %v", res.ReposFiled, res.StoppedAtCap)
	}

	f2, s2, p2 := testSetup(t)
	if _, err := Apply(context.Background(), s2, p2, ApplyOptions{Only: []string{"Rust"}, Out: io.Discard}); err != nil {
		t.Fatal(err)
	}
	if len(f2.lists) != 1 || f2.lists[0] != "Rust" {
		t.Errorf("-only applied the wrong categories: %v", f2.lists)
	}
}

// A repository already in a hand-curated list must stay in it.
func TestApplyKeepsUnrelatedLists(t *testing.T) {
	f, c, p := testSetup(t)
	f.lists = append(f.lists, "Read Later")
	f.member["helm/helm"] = []string{f.id("Read Later")}

	if _, err := Apply(context.Background(), c, p, ApplyOptions{Out: io.Discard}); err != nil {
		t.Fatal(err)
	}
	if !f.in("helm/helm", "Read Later") {
		t.Error("applying removed helm/helm from a list it was already in")
	}
	if !f.in("helm/helm", "Kubernetes") {
		t.Error("helm/helm was not added to its category")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	_, _, p := testSetup(t)
	p.Stamp(time.Now())
	path := filepath.Join(t.TempDir(), "nested", "plan.json")
	if err := p.Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.User != p.User || len(got.Categories) != len(p.Categories) || got.Categorized() != 3 {
		t.Errorf("round trip lost data: %+v", got)
	}
}

func TestLoadRejectsAnIncompatiblePlan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := writeFile(path, `{"version":99}`); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "incompatible") {
		t.Errorf("want an incompatible-version error, got %v", err)
	}
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

// GitHub caps an account at 32 star lists, and existing lists count towards
// it. Discovering that mid-run would leave the account half-organized.
func TestApplyRefusesToExceedGitHubsListCap(t *testing.T) {
	f, c, p := testSetup(t)
	for i := 0; i < gh.MaxLists-1; i++ {
		f.lists = append(f.lists, fmt.Sprintf("Existing %d", i))
	}
	// 31 existing + the plan's 2 new = 33, one over.
	_, err := Apply(context.Background(), c, p, ApplyOptions{Out: io.Discard})
	if err == nil {
		t.Fatal("want a refusal, got none")
	}
	for _, want := range []string{"32", "31", "--max-clusters"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
	if len(f.lists) != gh.MaxLists-1 {
		t.Errorf("a refused apply created %d lists, want none", len(f.lists)-(gh.MaxLists-1))
	}
}

// Exactly filling the cap is allowed.
func TestApplyFillsTheCapExactly(t *testing.T) {
	f, c, p := testSetup(t)
	for i := 0; i < gh.MaxLists-2; i++ {
		f.lists = append(f.lists, fmt.Sprintf("Existing %d", i))
	}
	if _, err := Apply(context.Background(), c, p, ApplyOptions{Out: io.Discard}); err != nil {
		t.Fatalf("30 existing + 2 new should fit in %d: %v", gh.MaxLists, err)
	}
	if len(f.lists) != gh.MaxLists {
		t.Errorf("ended with %d lists, want %d", len(f.lists), gh.MaxLists)
	}
}

// Categories that match a list already on the account cost no budget.
func TestApplyDoesNotChargeExistingListsTwice(t *testing.T) {
	f, c, p := testSetup(t)
	f.lists = append(f.lists, "Kubernetes")
	for i := 0; i < gh.MaxLists-2; i++ {
		f.lists = append(f.lists, fmt.Sprintf("Existing %d", i))
	}
	// 31 lists, one of which the plan reuses, so only "Rust" is new.
	if _, err := Apply(context.Background(), c, p, ApplyOptions{Out: io.Discard}); err != nil {
		t.Fatalf("reusing an existing list should not count against the cap: %v", err)
	}
}

// Reconciling must remove what the plan no longer asserts — and must not touch
// a list somebody made by hand.
func TestReconcileRemovesStalePlacementsAndLists(t *testing.T) {
	f, c, p := testSetup(t)
	// A list the tool made that the plan has since dropped, plus one it never made.
	f.lists = append(f.lists, "Retired", "Read Later")
	f.described["Retired"] = "Repositories about something. Grouped by constellation."
	f.described["Read Later"] = "my own list, hands off"
	f.member["helm/helm"] = []string{f.id("Retired"), f.id("Read Later")}

	res, err := Apply(context.Background(), c, p, ApplyOptions{Reconcile: true, Out: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ListsDeleted) != 1 || res.ListsDeleted[0] != "Retired" {
		t.Errorf("deleted %v, want just the dropped list", res.ListsDeleted)
	}
	if !contains(f.lists, "Read Later") {
		t.Error("a hand-made list was deleted")
	}
	if !f.in("helm/helm", "Read Later") {
		t.Error("reconciling removed a repository from a hand-made list")
	}
	if !f.in("helm/helm", "Kubernetes") {
		t.Error("the repository was not filed into its planned category")
	}
}

func contains(ss []string, s string) bool {
	for _, e := range ss {
		if e == s {
			return true
		}
	}
	return false
}

// A dry run must report the same decisions as a real one and change nothing.
// Reconciling deletes lists, so this is the only way to inspect it first.
func TestDryRunChangesNothing(t *testing.T) {
	f, c, p := testSetup(t)
	f.lists = append(f.lists, "Retired")
	f.described["Retired"] = "Repositories about nothing. Grouped by constellation."
	f.member["helm/helm"] = []string{f.id("Retired")}
	before := append([]string(nil), f.lists...)

	res, err := Apply(context.Background(), c, p, ApplyOptions{
		Reconcile: true, DryRun: true, Out: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ListsDeleted) != 1 || res.ListsDeleted[0] != "Retired" {
		t.Errorf("reported deletions %v, want the dropped list", res.ListsDeleted)
	}
	if len(res.ListsCreated) != 2 {
		t.Errorf("reported %d creations, want both planned lists", len(res.ListsCreated))
	}
	if res.ReposFiled != 3 {
		t.Errorf("reported %d placements, want 3", res.ReposFiled)
	}
	// Nothing may actually have happened.
	if len(f.lists) != len(before) {
		t.Errorf("lists changed during a dry run: %v -> %v", before, f.lists)
	}
	if !f.in("helm/helm", "Retired") {
		t.Error("a membership was removed during a dry run")
	}
	if f.posts != 0 {
		t.Errorf("a dry run sent %d writes", f.posts)
	}
}

// A repository in two categories must end up in both lists, and one mutation
// per repository must carry every membership it should have — the GraphQL
// mutation replaces the whole set, so an omission is a silent removal.
func TestApplyFilesOneRepoIntoTwoLists(t *testing.T) {
	f, c, p := testSetup(t)
	p.Categories[1].Repos = append(p.Categories[1].Repos, Repo{FullName: "helm/helm"})

	if _, err := Apply(context.Background(), c, p, ApplyOptions{Out: io.Discard}); err != nil {
		t.Fatal(err)
	}
	if !f.in("helm/helm", "Kubernetes") || !f.in("helm/helm", "Rust") {
		t.Errorf("helm/helm should be in both lists, got %v", f.member["helm/helm"])
	}
}

func TestApplySkipsFiledReposWithoutWriting(t *testing.T) {
	f, c, p := testSetup(t)
	if _, err := Apply(context.Background(), c, p, ApplyOptions{Out: io.Discard}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.posts = 0
	f.mu.Unlock()

	res, err := Apply(context.Background(), c, p, ApplyOptions{Out: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if res.ReposFiled != 0 || res.ReposSkipped != 3 {
		t.Errorf("second run filed %d and skipped %d, want 0 and 3", res.ReposFiled, res.ReposSkipped)
	}
	// Descriptions are still brought in line, so this is not zero; what
	// matters is that no membership was submitted.
	if f.posts > len(p.Categories) {
		t.Errorf("wrote %d times on a no-op run, want at most one per category", f.posts)
	}
}

// Visibility is a property of every run, not only of creation: a list made
// public by an earlier run has to be flipped when the flag changes, and back
// again when it changes back. Enforcing it in one direction only is how the
// lists ended up private with no way to say otherwise.
func TestApplyEnforcesVisibilityOnEveryRun(t *testing.T) {
	f, c, p := testSetup(t)
	if _, err := Apply(context.Background(), c, p, ApplyOptions{Out: io.Discard}); err != nil {
		t.Fatal(err)
	}
	if f.private["Kubernetes"] {
		t.Errorf("lists should be created public by default")
	}
	for _, want := range []bool{true, false} {
		if _, err := Apply(context.Background(), c, p, ApplyOptions{Private: want, Out: io.Discard}); err != nil {
			t.Fatal(err)
		}
		for _, l := range f.lists {
			if f.private[l] != want {
				t.Errorf("list %q private = %v, want %v", l, f.private[l], want)
			}
		}
	}
}

// A dry run has to be able to say "already in place", not just "would file":
// the whole point is previewing the difference between the plan and the
// account, and reporting work that is already done as pending hides it.
func TestDryRunReportsWhatIsAlreadyFiled(t *testing.T) {
	f, c, p := testSetup(t)
	if _, err := Apply(context.Background(), c, p, ApplyOptions{Out: io.Discard}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.posts = 0
	f.mu.Unlock()

	var buf strings.Builder
	res, err := Apply(context.Background(), c, p, ApplyOptions{DryRun: true, Out: &buf})
	if err != nil {
		t.Fatal(err)
	}
	if res.ReposFiled != 0 || res.ReposSkipped != 3 {
		t.Errorf("dry run over a filed account reported %d to file and %d in place, want 0 and 3",
			res.ReposFiled, res.ReposSkipped)
	}
	if f.posts != 0 {
		t.Errorf("dry run wrote %d times", f.posts)
	}
	if strings.Contains(buf.String(), "would file") {
		t.Errorf("dry run offered to file repositories that are already filed:\n%s", buf.String())
	}
}

// The progress count has to describe work, not inventory: on a re-run almost
// every repository is already in place, and counting those makes the estimate
// wrong by the ratio between the two.
func TestProgressCountsOnlyWorkLeft(t *testing.T) {
	f, c, p := testSetup(t)
	if _, err := Apply(context.Background(), c, p, ApplyOptions{Out: io.Discard}); err != nil {
		t.Fatal(err)
	}
	// Undo one placement, leaving a single repository to file out of three.
	f.mu.Lock()
	f.member["k3s-io/k3s"] = nil
	f.mu.Unlock()

	var buf strings.Builder
	if _, err := Apply(context.Background(), c, p, ApplyOptions{Out: &buf}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "[1/1") {
		t.Errorf("progress should count the one repository that needs filing:\n%s", buf.String())
	}
}

// Interrupting a run makes every remaining request fail at once. Treating
// those as ordinary per-item failures turned one Ctrl-C into hundreds of
// logged errors and a summary that read as though the run had completed.
func TestCancellationStopsEvenWithContinueOnError(t *testing.T) {
	_, c, p := testSetup(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := Apply(ctx, c, p, ApplyOptions{ContinueOnError: true, Out: io.Discard})
	if err == nil {
		t.Fatal("a cancelled run reported success")
	}
	if res != nil && len(res.Errors) > 1 {
		t.Errorf("a cancelled run logged %d errors, want it to stop at the first", len(res.Errors))
	}
}

// A session that dies partway through a long apply must stop the run with a
// clear cause, not press on. --continue is meant for a single bad repository;
// an expired cookie fails every remaining write, so continuing would turn one
// lost login into thousands of identical errors and a run that looks like it
// finished. It has to surface as ErrLoggedOut regardless of --continue.
func TestApplyStopsWhenTheTokenCannotWrite(t *testing.T) {
	f, c, p := testSetup(t)
	f.failWith = "INSUFFICIENT_SCOPES"

	res, err := Apply(context.Background(), c, p, ApplyOptions{ContinueOnError: true, Out: io.Discard})
	if !errors.Is(err, gh.ErrNeedsUserScope) {
		t.Fatalf("a token without the user scope returned %v, want ErrNeedsUserScope", err)
	}
	// It should stop near where the session died, not log an error per
	// remaining repository.
	if res != nil && len(res.Errors) > 1 {
		t.Errorf("logged %d errors, want it to stop at the first", len(res.Errors))
	}
}

// The ETA line reads whole minutes when there is more than a minute to go and
// whole seconds below that. Rounding everything to the minute, as it first did,
// printed "~0s left" for the entire final minute of a run.
func TestETARounding(t *testing.T) {
	for _, tc := range []struct {
		in   time.Duration
		want time.Duration
	}{
		{20 * time.Second, 20 * time.Second},
		{200 * time.Millisecond, time.Second},
		{90 * time.Second, 2 * time.Minute},
		{5 * time.Minute, 5 * time.Minute},
	} {
		if got := roundETA(tc.in); got != tc.want {
			t.Errorf("roundETA(%s) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

// A hand-made list whose name happens to match a planned category must not be
// silently taken over: overwriting its description stamps it as tool-managed,
// after which a later --reconcile could delete it. By default apply skips it
// and reports the conflict; --adopt is the explicit opt-in.
func TestApplyDoesNotAdoptAHandMadeList(t *testing.T) {
	f, c, p := testSetup(t) // plan has a "Kubernetes" category
	f.lists = append(f.lists, "Kubernetes")
	f.described["Kubernetes"] = "my own curated list, hands off" // no marker
	f.member["fluxcd/flux2"] = []string{f.id("Kubernetes")}

	res, err := Apply(context.Background(), c, p, ApplyOptions{Out: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Conflicts) != 1 || res.Conflicts[0] != "Kubernetes" {
		t.Fatalf("conflicts = %v, want [Kubernetes]", res.Conflicts)
	}
	// Untouched: description kept, no plan repos filed into it, still curated.
	if f.described["Kubernetes"] != "my own curated list, hands off" {
		t.Errorf("hand-made description was overwritten: %q", f.described["Kubernetes"])
	}
	for _, r := range []string{"helm/helm", "k3s-io/k3s"} {
		if f.in(r, "Kubernetes") {
			t.Errorf("%s was filed into the hand-made list despite the conflict", r)
		}
	}
	if !f.in("fluxcd/flux2", "Kubernetes") {
		t.Error("the hand-made list lost its curated member")
	}
}

// --adopt is the deliberate override: the same run takes the list over, marks
// it, and files into it.
func TestApplyAdoptsWithTheFlag(t *testing.T) {
	f, c, p := testSetup(t)
	f.lists = append(f.lists, "Kubernetes")
	f.described["Kubernetes"] = "my own curated list"

	res, err := Apply(context.Background(), c, p, ApplyOptions{Adopt: true, Out: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Conflicts) != 0 {
		t.Errorf("with --adopt there should be no conflicts, got %v", res.Conflicts)
	}
	// The plan's description replaces the hand-made one (in production it
	// carries the marker; the test plan's does not, so assert the overwrite).
	if f.described["Kubernetes"] == "my own curated list" {
		t.Errorf("adopted list kept its old description; it should be overwritten")
	}
	if !f.in("helm/helm", "Kubernetes") {
		t.Error("with --adopt the plan repos should be filed into the list")
	}
}

// Reset deletes only the lists this tool created — those whose description
// carries the marker — and leaves hand-made lists alone. It is the undo for
// apply, so mistaking a hand-made list for one of ours would be data loss.
func TestResetDeletesOnlyOwnLists(t *testing.T) {
	f, c, _ := testSetup(t)
	f.lists = []string{"Kubernetes", "Rust", "Read Later"}
	f.described["Kubernetes"] = "Repositories about kubernetes. Grouped by constellation."
	f.described["Rust"] = "Repositories about rust. Grouped by constellation."
	f.described["Read Later"] = "my own list, hands off"

	res, err := Reset(context.Background(), c, "octocat", ResetOptions{Out: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Deleted) != 2 {
		t.Errorf("deleted %v, want the two marked lists", res.Deleted)
	}
	if len(res.Kept) != 1 || res.Kept[0] != "Read Later" {
		t.Errorf("kept %v, want [Read Later]", res.Kept)
	}
	if contains(f.lists, "Kubernetes") || contains(f.lists, "Rust") {
		t.Errorf("a tool-made list survived reset: %v", f.lists)
	}
	if !contains(f.lists, "Read Later") {
		t.Errorf("the hand-made list was deleted: %v", f.lists)
	}
}

// A dry-run reset reports the same deletions but removes nothing.
func TestResetDryRunDeletesNothing(t *testing.T) {
	f, c, _ := testSetup(t)
	f.lists = []string{"Kubernetes"}
	f.described["Kubernetes"] = "Repositories about kubernetes. Grouped by constellation."

	res, err := Reset(context.Background(), c, "octocat", ResetOptions{DryRun: true, Out: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Deleted) != 1 {
		t.Errorf("dry run should report 1 deletion, got %v", res.Deleted)
	}
	if !contains(f.lists, "Kubernetes") {
		t.Error("dry run actually deleted the list")
	}
}

// A plan records the tuning that produced it, so it can be explained and
// reproduced later without remembering the command line.
func TestPlanRecordsSettingsThroughSaveAndLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.json")

	p := &Plan{
		Version: version, User: "octocat", Backend: "lsa(150)", TotalStars: 2,
		Settings: map[string]string{"algorithm": "agglomerative", "max-clusters": "12"},
		Categories: []Category{
			{Name: "Kubernetes", Description: "k8s. " + cluster.DescriptionMarker,
				Repos: []Repo{{FullName: "helm/helm"}}},
		},
	}
	if err := p.Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Settings["algorithm"] != "agglomerative" || got.Settings["max-clusters"] != "12" {
		t.Errorf("settings did not survive the round trip: %v", got.Settings)
	}
}

// Verify re-reads the account and confirms the plan's placements really landed.
// The write path treats "406 with an empty body" as success — behaviour
// observed rather than documented — so reading back is the only real proof.
func TestVerifyPassesAfterApply(t *testing.T) {
	_, c, p := testSetup(t)
	if _, err := Apply(context.Background(), c, p, ApplyOptions{Out: io.Discard}); err != nil {
		t.Fatal(err)
	}
	v, err := Verify(context.Background(), c, p, nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !v.OK() {
		t.Errorf("verify failed after a successful apply: missing=%v lists=%v", v.Missing, v.MissingLists)
	}
	if v.Checked != 3 {
		t.Errorf("checked %d placements, want 3", v.Checked)
	}
}

// A placement that never landed has to be reported, not glossed over.
func TestVerifyReportsMissingPlacements(t *testing.T) {
	f, c, p := testSetup(t)
	if _, err := Apply(context.Background(), c, p, ApplyOptions{Out: io.Discard}); err != nil {
		t.Fatal(err)
	}
	// Simulate a write that reported success but did not take effect.
	f.mu.Lock()
	f.member["k3s-io/k3s"] = nil
	f.mu.Unlock()

	v, err := Verify(context.Background(), c, p, nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if v.OK() {
		t.Fatal("verify passed despite a missing placement")
	}
	if got := v.Missing["Kubernetes"]; len(got) != 1 || got[0] != "k3s-io/k3s" {
		t.Errorf("missing = %v, want [k3s-io/k3s] under Kubernetes", v.Missing)
	}
}

// A category with no list at all is a different failure and reported separately.
func TestVerifyReportsMissingLists(t *testing.T) {
	_, c, p := testSetup(t)
	v, err := Verify(context.Background(), c, p, nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.MissingLists) != 2 {
		t.Errorf("missing lists = %v, want both categories before any apply", v.MissingLists)
	}
}

// A run with nothing to write reads only the planned lists. Paging contents is
// the slowest part of starting an apply, and the planned lists alone settle
// whether there is anything to do.
//
// A run that does write must read everything — see
// TestApplyReadsEveryListBeforeWriting — so this asserts the cheap case only.
func TestNoOpApplyReadsOnlyThePlannedLists(t *testing.T) {
	f, c, p := testSetup(t)
	f.lists = append(f.lists, "Unrelated")
	f.described["Unrelated"] = "mine"
	unrelated := f.id("Unrelated")
	f.member["someone/else"] = []string{unrelated}

	// First run does the work; the second has nothing left to do.
	if _, err := Apply(context.Background(), c, p, ApplyOptions{Out: io.Discard}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.itemReads = map[string]int{}
	f.mu.Unlock()

	res, err := Apply(context.Background(), c, p, ApplyOptions{Out: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if res.ReposFiled != 0 {
		t.Fatalf("second run filed %d, expected a no-op", res.ReposFiled)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if n := f.itemReads[unrelated]; n != 0 {
		t.Errorf("a no-op run read an unplanned list's contents %d times, want 0", n)
	}
}

// Before writing anything, every list's contents must be known: the mutation
// replaces a repository's whole set of memberships, so a hand-curated list
// that was never read would be silently dropped from it.
func TestApplyReadsEveryListBeforeWriting(t *testing.T) {
	f, c, p := testSetup(t)
	f.lists = append(f.lists, "Read Later")
	f.described["Read Later"] = "mine"
	later := f.id("Read Later")
	f.member["helm/helm"] = []string{later}

	if _, err := Apply(context.Background(), c, p, ApplyOptions{Out: io.Discard}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.itemReads[later] == 0 {
		t.Error("wrote without reading a hand-made list, which would drop its membership")
	}
}

// Reconciling must read them all: a list absent from the plan is exactly the
// one whose contents decide what gets removed.
func TestReconcileReadsEveryListsContents(t *testing.T) {
	f, c, p := testSetup(t)
	f.lists = append(f.lists, "Retired")
	f.described["Retired"] = "Repositories about something. " + cluster.DescriptionMarker
	retired := f.id("Retired") // captured now: reconcile deletes the list
	f.member["helm/helm"] = []string{retired}

	if _, err := Apply(context.Background(), c, p, ApplyOptions{Reconcile: true, Out: io.Discard}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if n := f.itemReads[retired]; n == 0 {
		t.Error("reconcile did not read the contents of a list outside the plan")
	}
}

func TestApplyReadsTheAccountOnce(t *testing.T) {
	f, c, p := testSetup(t)
	f.lists = append(f.lists, "Unrelated One", "Unrelated Two")
	f.described["Unrelated One"] = "mine"
	f.described["Unrelated Two"] = "also mine"

	f.mu.Lock()
	f.listReads = 0
	f.mu.Unlock()

	if _, err := Apply(context.Background(), c, p, ApplyOptions{Out: io.Discard}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listReads != 1 {
		t.Errorf("read the account %d times, want 1", f.listReads)
	}
}

// --force makes room for a plan that does not fit, by deleting only the lists
// this tool created that the plan has dropped. A list made by hand, and a list
// the plan still contains, must survive — those are the two ways this could
// destroy something the user wanted.
func TestForceDeletesOnlyItsOwnDroppedLists(t *testing.T) {
	f, c, p := testSetup(t)
	// Fill the account to the cap: one hand-made list, the rest ours and all
	// dropped by the plan.
	f.lists = []string{"Read Later"}
	f.described["Read Later"] = "mine, hands off"
	for i := range gh.MaxLists - 1 {
		name := fmt.Sprintf("Old %d", i)
		f.lists = append(f.lists, name)
		f.described[name] = "Repositories about something. " + cluster.DescriptionMarker
	}

	res, err := Apply(context.Background(), c, p, ApplyOptions{Force: true, Out: io.Discard})
	if err != nil {
		t.Fatalf("--force did not make room: %v", err)
	}
	if len(res.ListsDeleted) == 0 {
		t.Error("nothing was deleted, yet the plan did not fit")
	}
	if !contains(f.lists, "Read Later") {
		t.Error("--force deleted a hand-made list")
	}
	for _, cat := range p.Categories {
		if !contains(f.lists, cat.Name) {
			t.Errorf("category %q was not created after making room", cat.Name)
		}
	}
	// Only as many as needed: the account should still be at the cap, not
	// emptied out.
	if len(f.lists) > gh.MaxLists {
		t.Errorf("ended with %d lists, over the cap", len(f.lists))
	}
	if len(res.ListsDeleted) > len(p.Categories) {
		t.Errorf("deleted %d lists to make room for %d categories", len(res.ListsDeleted), len(p.Categories))
	}
}

// Without --force the same situation must refuse to start, naming the remedy
// rather than silently deleting.
func TestWithoutForceAFullAccountRefuses(t *testing.T) {
	f, c, p := testSetup(t)
	for i := range gh.MaxLists {
		name := fmt.Sprintf("Old %d", i)
		f.lists = append(f.lists, name)
		f.described[name] = "Repositories about something. " + cluster.DescriptionMarker
	}
	_, err := Apply(context.Background(), c, p, ApplyOptions{Out: io.Discard})
	if err == nil {
		t.Fatal("a full account was accepted without --force")
	}
	if !strings.Contains(err.Error(), "star lists per account") {
		t.Errorf("error does not explain the cap: %v", err)
	}
	if len(f.lists) != gh.MaxLists {
		t.Error("something was deleted without --force")
	}
}

// A dry run must report the deletions it would make and perform none of them.
func TestForceDryRunDeletesNothing(t *testing.T) {
	f, c, p := testSetup(t)
	for i := range gh.MaxLists {
		name := fmt.Sprintf("Old %d", i)
		f.lists = append(f.lists, name)
		f.described[name] = "Repositories about something. " + cluster.DescriptionMarker
	}
	before := len(f.lists)

	var buf strings.Builder
	res, err := Apply(context.Background(), c, p, ApplyOptions{Force: true, DryRun: true, Out: &buf})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ListsDeleted) == 0 {
		t.Error("dry run reported no deletions, but room was needed")
	}
	if len(f.lists) != before {
		t.Errorf("dry run actually deleted lists: %d -> %d", before, len(f.lists))
	}
	if !strings.Contains(buf.String(), "to make room") {
		t.Errorf("dry run did not say why it would delete:\n%s", buf.String())
	}
}

// Reconciling frees room by itself, because it deletes the categories the plan
// has dropped. The budget must therefore be checked after reconciling, not
// before — checking first made a plan that would have fit refuse to start.
func TestReconcileFreesRoomBeforeTheBudgetCheck(t *testing.T) {
	f, c, p := testSetup(t)
	for i := range gh.MaxLists {
		name := fmt.Sprintf("Old %d", i)
		f.lists = append(f.lists, name)
		f.described[name] = "Repositories about something. " + cluster.DescriptionMarker
	}
	res, err := Apply(context.Background(), c, p, ApplyOptions{Reconcile: true, Out: io.Discard})
	if err != nil {
		t.Fatalf("reconcile should have freed room: %v", err)
	}
	for _, cat := range p.Categories {
		if !contains(f.lists, cat.Name) {
			t.Errorf("category %q was not created", cat.Name)
		}
	}
	if len(res.ListsDeleted) != gh.MaxLists {
		t.Errorf("deleted %d dropped lists, want all %d", len(res.ListsDeleted), gh.MaxLists)
	}
}

// Batching must be a pure optimisation: GraphQL runs batched mutations
// serially, so the account must end up in exactly the state one-at-a-time
// writing would produce. Only the number of requests may differ.
func TestBatchingMatchesOneAtATime(t *testing.T) {
	state := func(batch int) (map[string][]string, int) {
		f, c, p := testSetup(t)
		// A repository in a hand-made list, to prove batching still carries
		// memberships the plan knows nothing about.
		f.lists = append(f.lists, "Read Later")
		f.described["Read Later"] = "mine"
		f.member["helm/helm"] = []string{f.id("Read Later")}

		if _, err := Apply(context.Background(), c, p, ApplyOptions{Batch: batch, Out: io.Discard}); err != nil {
			t.Fatal(err)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		got := map[string][]string{}
		for _, name := range f.lists {
			got[name] = f.reposIn(f.id(name))
		}
		return got, f.posts
	}

	one, onePosts := state(1)
	many, manyPosts := state(50)

	if !reflect.DeepEqual(one, many) {
		t.Errorf("batching changed the outcome:\n one-at-a-time %v\n batched       %v", one, many)
	}
	if manyPosts >= onePosts {
		t.Errorf("batching did not reduce requests: %d batched vs %d one-at-a-time", manyPosts, onePosts)
	}
}

// A batch that is larger than the work must still write everything: the final
// flush is easy to forget, and losing it would silently file nothing.
func TestBatchLargerThanTheWorkStillWrites(t *testing.T) {
	f, c, p := testSetup(t)
	if _, err := Apply(context.Background(), c, p, ApplyOptions{Batch: 1000, Out: io.Discard}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ repo, list string }{
		{"helm/helm", "Kubernetes"}, {"k3s-io/k3s", "Kubernetes"}, {"tokio-rs/tokio", "Rust"},
	} {
		if !f.in(tc.repo, tc.list) {
			t.Errorf("%s was not filed under %s", tc.repo, tc.list)
		}
	}
}

// A dry run has to model the run it is previewing. Reconciling deletes the
// categories the plan dropped, which is what frees room for the new ones — so
// a preview that kept the pre-reconcile picture reported those deletions and
// then refused for lack of the very room they would have freed.
func TestDryRunReconcileSeesTheRoomItWouldFree(t *testing.T) {
	f, c, p := testSetup(t)
	for i := range gh.MaxLists {
		name := fmt.Sprintf("Old %d", i)
		f.lists = append(f.lists, name)
		f.described[name] = "Repositories about something. " + cluster.DescriptionMarker
	}
	before := len(f.lists)

	res, err := Apply(context.Background(), c, p, ApplyOptions{Reconcile: true, DryRun: true, Out: io.Discard})
	if err != nil {
		t.Fatalf("dry run refused a plan that would fit: %v", err)
	}
	if len(res.ListsDeleted) != gh.MaxLists {
		t.Errorf("reported %d deletions, want %d", len(res.ListsDeleted), gh.MaxLists)
	}
	if len(res.ListsCreated) != len(p.Categories) {
		t.Errorf("reported %d creations, want %d", len(res.ListsCreated), len(p.Categories))
	}
	if len(f.lists) != before {
		t.Errorf("the dry run changed the account: %d -> %d lists", before, len(f.lists))
	}
}

// --limit is documented as making a first run cautious, so it has to bound
// every write. It counted only additions, which meant reconciling could remove
// placements without limit — and delete lists before the loop it guards even
// began.
func TestLimitCountsRemovalsToo(t *testing.T) {
	_, c, p := testSetup(t)
	if _, err := Apply(context.Background(), c, p, ApplyOptions{Out: io.Discard}); err != nil {
		t.Fatal(err)
	}
	// Drop a category from the plan so its members become stale placements.
	p.Categories = p.Categories[:1]

	res, err := Apply(context.Background(), c, p, ApplyOptions{Limit: 1, Out: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if total := res.ReposFiled + res.ReposRemoved; total > 1 {
		t.Errorf("wrote %d repositories under --limit 1", total)
	}
}

// Reconciling deletes lists before the write loop starts, so no limit on that
// loop can bound it. The combination is refused rather than pretending to be
// a safety belt.
func TestLimitRefusesToPretendItBoundsReconcile(t *testing.T) {
	_, c, p := testSetup(t)
	for _, opt := range []ApplyOptions{
		{Limit: 1, Reconcile: true, Out: io.Discard},
		{Limit: 1, Force: true, Out: io.Discard},
	} {
		_, err := Apply(context.Background(), c, p, opt)
		if err == nil {
			t.Errorf("--limit with reconcile=%v force=%v was accepted", opt.Reconcile, opt.Force)
			continue
		}
		if !strings.Contains(err.Error(), "--dry-run") {
			t.Errorf("error should point at the safe alternative: %v", err)
		}
	}
}

// Adopting replaces a hand-written description with the plan's, and that
// description is what marks a list as this tool's — so the list becomes
// subject to --reconcile and reset. Neither follows from the flag's name, so
// both are announced and recorded.
func TestAdoptAnnouncesWhatItCosts(t *testing.T) {
	f, c, p := testSetup(t)
	f.lists = append(f.lists, "Kubernetes")
	f.described["Kubernetes"] = "my own curated description"

	var buf strings.Builder
	res, err := Apply(context.Background(), c, p, ApplyOptions{Adopt: true, Out: &buf})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Adopted) != 1 || res.Adopted[0] != "Kubernetes" {
		t.Errorf("adopted = %v, want [Kubernetes]", res.Adopted)
	}
	out := buf.String()
	if !strings.Contains(out, "description will be replaced") || !strings.Contains(out, "reset") {
		t.Errorf("adoption did not say what it costs:\n%s", out)
	}
}

// An adopted list counts as ours immediately, or reconciling in the same run
// would announce that it is leaving alone the list it is about to take over.
func TestAdoptedListIsManagedInTheSameRun(t *testing.T) {
	f, c, p := testSetup(t)
	f.lists = append(f.lists, "Kubernetes")
	f.described["Kubernetes"] = "mine"

	var buf strings.Builder
	if _, err := Apply(context.Background(), c, p, ApplyOptions{Adopt: true, Reconcile: true, Out: &buf}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "leaving Kubernetes alone") {
		t.Errorf("reconcile disowned a list being adopted in the same run:\n%s", buf.String())
	}
}

// --only names categories to apply, so reconciling under it must stay inside
// them: bring the named lists in line, and delete nothing — a category the
// plan has dropped is not one --only can name, so deleting it would make the
// flag mean far more than it says.
func TestOnlyScopesReconcile(t *testing.T) {
	f, c, p := testSetup(t)
	if _, err := Apply(context.Background(), c, p, ApplyOptions{Out: io.Discard}); err != nil {
		t.Fatal(err)
	}
	// A managed list the plan no longer contains, plus a stale placement in a
	// category the run will not name.
	f.lists = append(f.lists, "Retired")
	f.described["Retired"] = "Repositories about something. " + cluster.DescriptionMarker
	f.member["tokio-rs/tokio"] = append(f.member["tokio-rs/tokio"], f.id("Retired"))
	strayInRust := f.id("Rust")
	f.member["helm/helm"] = append(f.member["helm/helm"], strayInRust)

	res, err := Apply(context.Background(), c, p, ApplyOptions{
		Reconcile: true, Only: []string{"Kubernetes"}, Out: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ListsDeleted) != 0 {
		t.Errorf("a scoped reconcile deleted %v", res.ListsDeleted)
	}
	if !contains(f.lists, "Retired") {
		t.Error("a scoped reconcile deleted a list outside the selection")
	}
	// helm/helm does not belong in Rust, but Rust was not named, so it stays.
	if !f.in("helm/helm", "Rust") {
		t.Error("a scoped reconcile stripped a placement from a list it was not asked to touch")
	}
}

// Unscoped, the same situation is reconciled in full — otherwise --only would
// have quietly become the default.
func TestReconcileWithoutOnlyIsStillAccountWide(t *testing.T) {
	f, c, p := testSetup(t)
	if _, err := Apply(context.Background(), c, p, ApplyOptions{Out: io.Discard}); err != nil {
		t.Fatal(err)
	}
	f.lists = append(f.lists, "Retired")
	f.described["Retired"] = "Repositories about something. " + cluster.DescriptionMarker

	res, err := Apply(context.Background(), c, p, ApplyOptions{Reconcile: true, Out: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ListsDeleted) != 1 || res.ListsDeleted[0] != "Retired" {
		t.Errorf("deleted %v, want the dropped list", res.ListsDeleted)
	}
}

// Freeing room has no narrower meaning: the lists it would delete are exactly
// the ones --only cannot name, so the combination is refused.
func TestForceRefusesToBeScoped(t *testing.T) {
	_, c, p := testSetup(t)
	_, err := Apply(context.Background(), c, p, ApplyOptions{
		Force: true, Only: []string{"Kubernetes"}, Out: io.Discard,
	})
	if err == nil {
		t.Fatal("--force with --only was accepted")
	}
	if !strings.Contains(err.Error(), "not asked to touch") {
		t.Errorf("error does not explain why: %v", err)
	}
}
