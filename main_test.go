package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mrueg/constellation/internal/gh"
	"github.com/mrueg/constellation/internal/plan"
	"github.com/mrueg/constellation/internal/textproc"
	"github.com/urfave/cli/v3"
)

// -only accepts both a repeated flag and a comma-separated list, since the
// category names it refers to are themselves multi-word.
func TestApplyFlagsCategories(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want []string
	}{
		{nil, nil},
		{[]string{"Helm"}, []string{"Helm"}},
		{[]string{"Helm,Rust"}, []string{"Helm", "Rust"}},
		{[]string{"Helm, Rust", "Kubernetes"}, []string{"Helm", "Rust", "Kubernetes"}},
		{[]string{"Secrets & Vault,GitHub & Actions"}, []string{"Secrets & Vault", "GitHub & Actions"}},
		{[]string{" , "}, nil},
	} {
		a := &applyFlags{only: tc.in}
		if got := a.categories(); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("categories(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The command tree must be well-formed; urfave validates names and flags when
// it builds help.
func TestCommandTree(t *testing.T) {
	root := command()
	// No default command: a bare invocation must not silently start planning,
	// which reads every star and can write a plan file. It shows help instead.
	if root.DefaultCommand != "" {
		t.Errorf("default command = %q, want none so a bare call does not run plan", root.DefaultCommand)
	}
	for _, want := range []string{"plan", "apply", "show", "reset"} {
		if root.Command(want) == nil {
			t.Errorf("missing %q command", want)
		}
	}
	if len(root.Command("plan").Flags) == 0 {
		t.Error("plan command has no flags")
	}
}

// Running with no subcommand must not fall through to plan. It should print
// help and change nothing, so a plan is written only when planning is asked
// for by name.
func TestNoSubcommandDoesNotPlan(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "constellation-plan.json")

	var out strings.Builder
	cmd := command()
	cmd.Writer = &out
	if err := cmd.Run(context.Background(), []string{"constellation"}); err != nil {
		t.Fatalf("bare invocation errored: %v", err)
	}
	if _, err := os.Stat(planPath); !os.IsNotExist(err) {
		t.Errorf("a bare invocation wrote a plan file; it should only show help")
	}
	if !strings.Contains(out.String(), "USAGE") && !strings.Contains(out.String(), "COMMANDS") {
		t.Errorf("a bare invocation did not print help:\n%s", out.String())
	}
}

// defaultPlanFlags fills a planFlags the way the CLI does, by letting urfave
// apply the flag defaults. Constructing one by hand instead would test a
// second set of defaults that nothing else uses.
func defaultPlanFlags(t *testing.T) *planFlags {
	t.Helper()
	f := &planFlags{}
	cmd := &cli.Command{
		Name:   "plan",
		Flags:  f.flags(),
		Action: func(context.Context, *cli.Command) error { return nil },
	}
	if err := cmd.Run(context.Background(), []string{"plan"}); err != nil {
		t.Fatal(err)
	}
	return f
}

func synthRepos(n int) []gh.Repo {
	kinds := []struct {
		lang   string
		topics []string
		desc   string
	}{
		{"Go", []string{"kubernetes", "operator"}, "a kubernetes operator for stateful workloads"},
		{"Rust", []string{"cli", "terminal"}, "a fast terminal user interface toolkit"},
		{"Python", []string{"machine-learning"}, "training and serving transformer models"},
	}
	repos := make([]gh.Repo, n)
	for i := range repos {
		k := kinds[i%len(kinds)]
		repos[i] = gh.Repo{
			ID:          int64(i + 1),
			FullName:    fmt.Sprintf("owner%d/repo%d", i, i),
			Description: k.desc,
			Language:    k.lang,
			Topics:      k.topics,
		}
	}
	return repos
}

// The whole pipeline has to hold together on collections far smaller than the
// number of categories asked for: a new account, or a heavily filtered one.
// Naming and refinement are the stages that assume a cluster has members.
func TestBuildPlanSurvivesTinyCollections(t *testing.T) {
	f := defaultPlanFlags(t)
	for _, n := range []int{0, 1, 2, 5, 12, 60} {
		p, err := buildPlan(context.Background(), nil, "octocat", synthRepos(n), f)
		if err != nil {
			// Too little text to build a vocabulary from is a fair answer, as
			// long as it arrives as an error naming the remedy rather than as
			// a panic or an empty plan presented as a real one.
			if !strings.Contains(err.Error(), "--") {
				t.Errorf("n=%d: unhelpful failure: %v", n, err)
			}
			t.Logf("n=%d: %v", n, err)
			continue
		}
		seen := map[string]bool{}
		placed := 0
		for _, c := range p.Categories {
			if c.Name == "" {
				t.Errorf("n=%d: a category came out unnamed", n)
			}
			if seen[c.Name] {
				t.Errorf("n=%d: duplicate category %q", n, c.Name)
			}
			seen[c.Name] = true
			placed += len(c.Repos)
		}
		if placed > n {
			t.Errorf("n=%d: %d placements from %d repositories", n, placed, n)
		}
	}
}

// The same input must produce the same plan. Determinism is what makes a
// re-run safe to apply: a plan that reshuffles on every run would move
// repositories between lists for no reason.
func TestBuildPlanIsDeterministic(t *testing.T) {
	f := defaultPlanFlags(t)
	repos := synthRepos(60)
	first, err := buildPlan(context.Background(), nil, "octocat", repos, f)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		again, err := buildPlan(context.Background(), nil, "octocat", repos, f)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(first.Categories, again.Categories) {
			t.Fatal("two runs over the same input produced different categories")
		}
	}
}

// One knob scales the three inferred-topic sources together, so their measured
// ratio has to survive it, and zero has to mean zero rather than a term with
// no weight — which is what made the log of it diverge.
func TestWeightsScaleInferredTopicsTogether(t *testing.T) {
	f := defaultPlanFlags(t)
	f.topicInferred = 2
	w := weights(f)
	if w.TopicFromName != 2*textproc.WeightTopicFromName ||
		w.TopicFromText != 2*textproc.WeightTopicFromText ||
		w.TopicFromReadme != 2*textproc.WeightTopicFromReadme {
		t.Errorf("scaling did not keep the ratio: %+v", w)
	}
	f.topicInferred = 0
	if w := weights(f); w.TopicFromName != 0 || w.TopicFromText != 0 || w.TopicFromReadme != 0 {
		t.Errorf("zero should switch inferred topics off entirely: %+v", w)
	}
}

// fetchReadmes must return the number of READMEs it actually stored, on both
// the normal and the interrupted path. The earlier code returned the failure
// count on interrupt, so runPlan's "if fetched > 0, save the cache" gate
// misfired and a Ctrl-C after fetching hundreds of READMEs discarded them all.
type stubReadmes struct {
	text string
	err  error
}

func (s stubReadmes) Readme(ctx context.Context, owner, repo string, limit int) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return s.text, s.err
}

func TestFetchReadmesReturnsSuccessCount(t *testing.T) {
	repos := []gh.Repo{{FullName: "a/one"}, {FullName: "b/two"}, {FullName: "c/three"}}
	f := &planFlags{readmeWorker: 2, readmeBytes: 1000, readmeWords: 50}

	if got := fetchReadmes(context.Background(), stubReadmes{text: "hello world"}, repos, f); got != 3 {
		t.Errorf("fetched %d, want 3", got)
	}
	for i := range repos {
		if repos[i].Readme == "" {
			t.Errorf("%s got no README stored", repos[i].FullName)
		}
	}
}

// A cancelled context must store nothing and report zero — not a mislabelled
// failure count, which is what made the cache-save gate in runPlan misfire.
func TestFetchReadmesOnCancelReturnsZero(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	repos := []gh.Repo{{FullName: "a/one"}, {FullName: "b/two"}}
	f := &planFlags{readmeWorker: 2, readmeBytes: 1000, readmeWords: 50}

	if got := fetchReadmes(ctx, stubReadmes{text: "hi"}, repos, f); got != 0 {
		t.Errorf("cancelled fetch returned %d, want 0", got)
	}
	for i := range repos {
		if repos[i].Readme != "" {
			t.Errorf("%s stored a README despite cancellation", repos[i].FullName)
		}
	}
}

// The category limit is also readable from the environment, for callers that
// cannot pass a flag — a screen recording, a wrapper script. An explicit flag
// still wins, which is the usual precedence and the one people expect.
func TestShowReadsTheEnvironment(t *testing.T) {
	for _, tc := range []struct {
		env  string
		args []string
		want int
	}{
		{"", []string{"plan"}, 0},
		{"3", []string{"plan"}, 3},
		{"3", []string{"plan", "--show", "1"}, 1},
	} {
		t.Setenv(envShow, tc.env)
		if tc.env == "" {
			t.Setenv(envShow, "")
		}
		f := &planFlags{}
		cmd := &cli.Command{
			Name:   "plan",
			Flags:  f.flags(),
			Action: func(context.Context, *cli.Command) error { return nil },
		}
		if err := cmd.Run(context.Background(), tc.args); err != nil {
			t.Fatal(err)
		}
		if f.show != tc.want {
			t.Errorf("%s=%q %v gave show=%d, want %d", envShow, tc.env, tc.args, f.show, tc.want)
		}
	}
}

// A README that cannot be read now will not become readable later — over a
// megabyte, blocked, taken down. Leaving it unmarked made every future run
// spend a request rediscovering that, forever.
func TestPermanentReadmeFailuresAreNotRetriedForever(t *testing.T) {
	repos := []gh.Repo{{FullName: "a/one"}, {FullName: "b/two"}}
	f := &planFlags{readmeWorker: 2, readmeBytes: 1000, readmeWords: 50}

	got := fetchReadmes(context.Background(), stubReadmes{err: errors.New("file too large")}, repos, f)
	if got != 0 {
		t.Errorf("fetched %d, want 0", got)
	}
	for i := range repos {
		if repos[i].Readme == "" {
			t.Errorf("%s was left unmarked, so the next run would try it again", repos[i].FullName)
		}
	}
}

// An interruption is not a permanent failure: those repositories must stay on
// the list so a later run picks them up.
func TestInterruptedReadmesStayUnmarked(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	repos := []gh.Repo{{FullName: "a/one"}, {FullName: "b/two"}}
	f := &planFlags{readmeWorker: 2, readmeBytes: 1000, readmeWords: 50}
	fetchReadmes(ctx, stubReadmes{text: "hi"}, repos, f)

	for i := range repos {
		if repos[i].Readme != "" {
			t.Errorf("%s was marked despite the run being interrupted", repos[i].FullName)
		}
	}
}

// countingReadmes records how many repositories were actually asked for, which
// is the cost this cache exists to avoid.
type countingReadmes struct {
	calls int
	mu    sync.Mutex
}

func (c *countingReadmes) Readme(_ context.Context, _, _ string, _ int) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return "a readme", nil
}

// Re-fetching the star list must not re-fetch every README with it. The star
// cache expires in a day and a README costs an API call each, so holding both
// under one expiry made picking up a handful of new stars cost thousands of
// requests.
func TestCachedReadmesSurviveAStarRefresh(t *testing.T) {
	repos := []gh.Repo{{FullName: "a/one"}, {FullName: "b/two"}, {FullName: "c/three"}}
	cache := gh.ReadmeCache{
		"a/one": {Text: "one", FetchedAt: time.Now()},
		"b/two": {Text: "two", FetchedAt: time.Now()},
	}
	applyReadmes(cache, repos)

	f := &planFlags{readmeWorker: 2, readmeBytes: 1000, readmeWords: 50}
	c := &countingReadmes{}
	if got := fetchReadmes(context.Background(), c, repos, f); got != 1 {
		t.Errorf("fetched %d READMEs, want the 1 that was not cached", got)
	}
	if c.calls != 1 {
		t.Errorf("%d API calls, want 1: the cached READMEs were re-read", c.calls)
	}
	if repos[0].Readme != "one" || repos[1].Readme != "two" {
		t.Errorf("cached text did not reach the repositories: %q, %q", repos[0].Readme, repos[1].Readme)
	}

	now := time.Now()
	if changed := harvestReadmes(cache, repos, now); changed != 1 {
		t.Errorf("harvested %d entries, want the 1 newly read one", changed)
	}
	if cache["c/three"].Text != "a readme" {
		t.Errorf("the newly read README was not cached: %q", cache["c/three"].Text)
	}
	// A second run with nothing new must leave the file alone.
	if changed := harvestReadmes(cache, repos, now); changed != 0 {
		t.Errorf("harvested %d entries from an unchanged run, want 0", changed)
	}
}

// A README that cannot be read is marked so that future runs stop trying, and
// that verdict has to reach the cache even though nothing was fetched — the
// old "save only if something was fetched" gate dropped exactly these.
func TestUnreadableReadmesAreCached(t *testing.T) {
	repos := []gh.Repo{{FullName: "a/one"}, {FullName: "b/two"}}
	f := &planFlags{readmeWorker: 2, readmeBytes: 1000, readmeWords: 50}

	if got := fetchReadmes(context.Background(), stubReadmes{err: errors.New("file too large")}, repos, f); got != 0 {
		t.Fatalf("fetched %d, want 0", got)
	}
	cache := gh.ReadmeCache{}
	if changed := harvestReadmes(cache, repos, time.Now()); changed != 2 {
		t.Errorf("harvested %d entries, want both failures recorded", changed)
	}
}

// placements maps a repository to the categories it is filed under.
func placements(p *plan.Plan) map[string][]string {
	out := map[string][]string{}
	for _, c := range p.Categories {
		for _, r := range c.Repos {
			out[r.FullName] = append(out[r.FullName], c.Name)
		}
	}
	return out
}

// An incremental run files the new stars and changes nothing else. Re-running
// the whole model to place a handful of new repositories moves ones that were
// reviewed and applied weeks ago, because the categories are one reasonable
// cut of several and the cut moves when the corpus does.
func TestExtendPlanOnlyPlacesTheNewStars(t *testing.T) {
	f := defaultPlanFlags(t)
	repos := synthRepos(60)
	first, err := buildPlan(context.Background(), nil, "octocat", repos, f)
	if err != nil {
		t.Fatal(err)
	}
	first.Stamp(time.Now())

	// synthRepos is a prefix generator, so these are the same 60 plus 6 new.
	grown := synthRepos(66)
	second, err := extendPlan(context.Background(), nil, "octocat", grown, f, first)
	if err != nil {
		t.Fatal(err)
	}

	if len(second.Categories) != len(first.Categories) {
		t.Fatalf("categories went from %d to %d; an incremental run must not add or drop any",
			len(first.Categories), len(second.Categories))
	}
	for i := range first.Categories {
		if first.Categories[i].Name != second.Categories[i].Name {
			t.Errorf("category %d was renamed from %q to %q", i, first.Categories[i].Name, second.Categories[i].Name)
		}
		if first.Categories[i].Description != second.Categories[i].Description {
			t.Errorf("category %q lost its description", first.Categories[i].Name)
		}
	}

	before, after := placements(first), placements(second)
	for name, was := range before {
		if !reflect.DeepEqual(was, after[name]) {
			t.Errorf("%s moved from %v to %v", name, was, after[name])
		}
	}

	newcomers := 0
	for _, r := range grown[60:] {
		if len(after[r.FullName]) > 0 {
			newcomers++
		}
	}
	unassigned := map[string]bool{}
	for _, r := range second.Unassigned {
		unassigned[r.FullName] = true
	}
	for _, r := range grown[60:] {
		if len(after[r.FullName]) == 0 && !unassigned[r.FullName] {
			t.Errorf("%s was neither filed nor reported as uncategorized", r.FullName)
		}
	}
	if newcomers == 0 {
		t.Error("none of the six new repositories was filed, though they repeat the same three kinds")
	}
	if second.Settings["incremental"] != "true" {
		t.Error("the plan does not record that it was extended rather than built")
	}
}

// A plan is tied to the account it was made for: extending somebody else's
// plan would file their categories onto this account's stars.
func TestExtendPlanRefusesAnotherAccountsPlan(t *testing.T) {
	f := defaultPlanFlags(t)
	repos := synthRepos(60)
	p, err := buildPlan(context.Background(), nil, "octocat", repos, f)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := extendPlan(context.Background(), nil, "someone-else", repos, f, p); err == nil {
		t.Error("extending a plan made for another account was allowed")
	}
}
