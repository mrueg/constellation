package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
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
	for _, algorithm := range []string{"agglomerative", "kmeans"} {
		f := defaultPlanFlags(t)
		f.algorithm = algorithm
		for _, n := range []int{0, 1, 2, 3, 5, 12, 60} {
			label := fmt.Sprintf("%s n=%d", algorithm, n)
			p, err := buildPlan(context.Background(), nil, "octocat", synthRepos(n), f)
			if err != nil {
				// Too little text to build a vocabulary from is a fair answer,
				// as long as it arrives as an error naming the remedy rather
				// than as a panic or an empty plan presented as a real one.
				if !strings.Contains(err.Error(), "--") {
					t.Errorf("%s: unhelpful failure: %v", label, err)
				}
				t.Logf("%s: %v", label, err)
				continue
			}
			seen := map[string]bool{}
			placed := 0
			for _, c := range p.Categories {
				if c.Name == "" {
					t.Errorf("%s: a category came out unnamed", label)
				}
				if seen[c.Name] {
					t.Errorf("%s: duplicate category %q", label, c.Name)
				}
				seen[c.Name] = true
				placed += len(c.Repos)
			}
			if placed > n {
				t.Errorf("%s: %d placements from %d repositories", label, placed, n)
			}
		}
	}
}

// Under the default vocabulary floor a handful of repositories never reaches
// the clustering at all, so the k-means path is exercised here with the
// floor lifted: the silhouette sweep used to come back empty on one, two or
// three repositories, and the refinement step then dereferenced nothing.
func TestBuildPlanKMeansSurvivesTinyCollections(t *testing.T) {
	for _, consensus := range []int{1, 3} {
		f := defaultPlanFlags(t)
		f.algorithm = "kmeans"
		f.consensus = consensus
		f.minDF, f.maxVocab = 1, 0
		for _, n := range []int{1, 2, 3} {
			label := fmt.Sprintf("consensus=%d n=%d", consensus, n)
			p, err := buildPlan(context.Background(), nil, "octocat", synthRepos(n), f)
			if err != nil {
				if !strings.Contains(err.Error(), "--") {
					t.Errorf("%s: unhelpful failure: %v", label, err)
				}
				t.Logf("%s: %v", label, err)
				continue
			}
			placed := 0
			for _, c := range p.Categories {
				placed += len(c.Repos)
			}
			if placed > n {
				t.Errorf("%s: %d placements from %d repositories", label, placed, n)
			}
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

func (s stubReadmes) Readme(ctx context.Context, _, _ string, _ gh.ReadmeLimits) (string, error) {
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
// spend a request rediscovering that, forever. The client says which failures
// those are; only they earn the mark.
func TestPermanentReadmeFailuresAreNotRetriedForever(t *testing.T) {
	repos := []gh.Repo{{FullName: "a/one"}, {FullName: "b/two"}}
	f := &planFlags{readmeWorker: 2, readmeBytes: 1000, readmeWords: 50}

	err := fmt.Errorf("%w: file too large", gh.ErrReadmeUnavailable)
	got := fetchReadmes(context.Background(), stubReadmes{err: err}, repos, f)
	if got != 0 {
		t.Errorf("fetched %d, want 0", got)
	}
	for i := range repos {
		if repos[i].Readme == "" {
			t.Errorf("%s was left unmarked, so the next run would try it again", repos[i].FullName)
		}
	}
}

// The opposite mistake is the expensive one. A timeout, a reset or a server
// error says nothing about the README, yet every failure used to be marked as
// unreadable and cached for a month: one bad hour poisoned thousands of
// entries. Those repositories must stay on the list, and the marker must not
// reach the cache.
func TestTransientReadmeFailuresStayUnmarked(t *testing.T) {
	for _, err := range []error{
		errors.New("dial tcp: i/o timeout"),
		&url.Error{Op: "Get", URL: "https://api.github.com/repos/a/one/readme", Err: io.ErrUnexpectedEOF},
	} {
		repos := []gh.Repo{{FullName: "a/one"}, {FullName: "b/two"}}
		f := &planFlags{readmeWorker: 2, readmeBytes: 1000, readmeWords: 50}

		if got := fetchReadmes(context.Background(), stubReadmes{err: err}, repos, f); got != 0 {
			t.Errorf("%v: fetched %d, want 0", err, got)
		}
		for i := range repos {
			if repos[i].Readme != "" {
				t.Errorf("%v: %s was marked %q, so a passing failure would be cached as permanent", err, repos[i].FullName, repos[i].Readme)
			}
		}
		if changed := harvestReadmes(gh.ReadmeCache{}, repos, time.Now()); changed != 0 {
			t.Errorf("%v: %d failures reached the cache, want none", err, changed)
		}
	}
}

// countingFailures fails every request the same way and counts them.
type countingFailures struct {
	err   error
	mu    sync.Mutex
	calls int
}

func (c *countingFailures) Readme(context.Context, string, string, gh.ReadmeLimits) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return "", c.err
}

// A rejected token or an exhausted rate limit fails every request from then
// on, and there is nothing to learn from sending a thousand more. The fetch
// stops handing out work once it sees one, and none of the repositories it
// did not reach is marked.
func TestFatalReadmeFailuresStopTheFetch(t *testing.T) {
	for _, fatal := range []error{gh.ErrUnauthorized, gh.ErrRateLimited} {
		repos := make([]gh.Repo, 50)
		for i := range repos {
			repos[i].FullName = fmt.Sprintf("owner%d/repo%d", i, i)
		}
		const workers = 2
		f := &planFlags{readmeWorker: workers, readmeBytes: 1000, readmeWords: 50}
		c := &countingFailures{err: fmt.Errorf("%w: GET /readme: 401", fatal)}

		if got := fetchReadmes(context.Background(), c, repos, f); got != 0 {
			t.Errorf("%v: fetched %d, want 0", fatal, got)
		}
		// Each worker may have had one request in flight when the first
		// verdict landed; anything beyond that was work knowingly wasted.
		if c.calls > workers {
			t.Errorf("%v: %d requests were sent after the first told the whole story, want at most %d", fatal, c.calls, workers)
		}
		for i := range repos {
			if repos[i].Readme != "" {
				t.Errorf("%v: %s was marked unreadable by a failure that was not about it", fatal, repos[i].FullName)
			}
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

func (c *countingReadmes) Readme(_ context.Context, _, _ string, _ gh.ReadmeLimits) (string, error) {
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

	err := fmt.Errorf("%w: file too large", gh.ErrReadmeUnavailable)
	if got := fetchReadmes(context.Background(), stubReadmes{err: err}, repos, f); got != 0 {
		t.Fatalf("fetched %d, want 0", got)
	}
	cache := gh.ReadmeCache{}
	if changed := harvestReadmes(cache, repos, time.Now()); changed != 2 {
		t.Errorf("harvested %d entries, want both failures recorded", changed)
	}
}

// countingStars stands in for GitHub and records which kind of read each run
// made: a full walk, or a top-up of the cached tail.
type countingStars struct {
	full, topUps int
}

func (c *countingStars) Starred(context.Context) ([]gh.Repo, error) {
	c.full++
	return []gh.Repo{{ID: 1, FullName: "a/one"}, {ID: 2, FullName: "b/two"}}, nil
}

func (c *countingStars) StarredIncremental(_ context.Context, cached []gh.Repo) ([]gh.Repo, bool, error) {
	c.topUps++
	return cached, true, nil
}

// ageStarCache moves every timestamp in the star cache back by d, which is
// what the file would look like had d passed since it was written: the only
// way to run the tool "tomorrow" without waiting for it.
func ageStarCache(t *testing.T, path string, d time.Duration) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var file map[string]json.RawMessage
	if err := json.Unmarshal(b, &file); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"fetched_at", "full_fetched_at"} {
		raw, ok := file[key]
		if !ok {
			continue
		}
		var at time.Time
		if err := json.Unmarshal(raw, &at); err != nil {
			t.Fatal(err)
		}
		if file[key], err = json.Marshal(at.Add(-d)); err != nil {
			t.Fatal(err)
		}
	}
	if b, err = json.Marshal(file); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A top-up never revisits the repositories already cached, so
// --cache-full-ttl promises a full re-read once in a while for the
// descriptions and topics edited upstream. Measuring that age from the last
// write, which every top-up refreshes, meant that running the tool daily
// kept the full re-read forever a month away.
func TestDailyTopUpsStillReachTheFullReRead(t *testing.T) {
	const day = 24 * time.Hour
	f := &planFlags{
		cachePath:    filepath.Join(t.TempDir(), "stars.json"),
		cacheMaxAge:  day,
		cacheFullAge: 30 * day,
	}
	src := &countingStars{}

	// Day 0: nothing cached, so the list is read in full.
	if _, err := loadStars(context.Background(), src, "octocat", f); err != nil {
		t.Fatal(err)
	}
	if src.full != 1 || src.topUps != 0 {
		t.Fatalf("first run made %d full reads and %d top-ups, want 1 and 0", src.full, src.topUps)
	}

	var fullOn int
	for d := 1; d <= 31; d++ {
		ageStarCache(t, f.cachePath, day)
		before := src.full
		if _, err := loadStars(context.Background(), src, "octocat", f); err != nil {
			t.Fatalf("day %d: %v", d, err)
		}
		if src.full > before {
			if fullOn != 0 {
				t.Fatalf("day %d: a second full read, after one on day %d", d, fullOn)
			}
			fullOn = d
		}
	}
	if fullOn == 0 {
		t.Fatalf("31 daily runs made %d top-ups and never re-read the list in full; --cache-full-ttl is 30 days", src.topUps)
	}
	if fullOn < 30 {
		t.Errorf("the full re-read came on day %d, before --cache-full-ttl had passed", fullOn)
	}
	if src.topUps != 30 {
		t.Errorf("%d top-ups over 31 days, want 30: every run but the full re-read", src.topUps)
	}
}

// A date is what someone reaches for once; an age is what a script wants,
// because it keeps meaning the same thing tomorrow. Go's own duration syntax
// stops at hours, which is far too short a unit for a star list.
func TestParseSince(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		in   string
		want time.Time
		bad  bool
	}{
		{"", time.Time{}, false},
		{"2024-01-01", time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), false},
		{"2024-01-01T10:00:00Z", time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC), false},
		{"2y", time.Date(2024, 9, 7, 12, 0, 0, 0, time.UTC), false},
		{"18mo", time.Date(2025, 3, 7, 12, 0, 0, 0, time.UTC), false},
		{"30d", time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC), false},
		{"12h", time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC), false},
		{"last tuesday", time.Time{}, true},
		{"yesterday", time.Time{}, true}, // ends in a "y" but is not an age
		{"xy", time.Time{}, true},
	} {
		got, err := parseSince(tc.in, now)
		if tc.bad {
			if err == nil {
				t.Errorf("parseSince(%q) = %v, want an error naming the accepted forms", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseSince(%q): %v", tc.in, err)
			continue
		}
		if !got.Equal(tc.want) {
			t.Errorf("parseSince(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// Every one of these was already fetched with the star list, so filtering on
// them costs nothing. What matters is that a filtered-out star is reported:
// silently working from two thirds of an account shows up only as categories
// that make no sense.
func TestRepoFilterApply(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	repos := []gh.Repo{
		{FullName: "a/keep", Stars: 400, StarredAt: now.AddDate(0, -1, 0)},
		{FullName: "b/forked", Fork: true, Stars: 400},
		{FullName: "c/archived", Archived: true, Stars: 400},
		{FullName: "d/tiny", Stars: 3, StarredAt: now},
		{FullName: "e/ancient", Stars: 900, StarredAt: now.AddDate(-4, 0, 0)},
		{FullName: "torvalds/linux", Stars: 190000, StarredAt: now},
		{FullName: "someone/awesome-go", Stars: 100, StarredAt: now},
	}
	f := &planFlags{
		minStars:     10,
		starredAfter: "2y",
		exclude:      []string{"torvalds/*", "awesome-*"},
	}
	rf, err := f.filter(now)
	if err != nil {
		t.Fatal(err)
	}
	got := rf.apply(repos)
	if len(got) != 1 || got[0].FullName != "a/keep" {
		names := make([]string, len(got))
		for i, r := range got {
			names[i] = r.FullName
		}
		t.Errorf("kept %v, want only a/keep", names)
	}
}

// A glob or a date that cannot be read must stop the run before it costs a
// walk through every page of stars.
func TestFilterRejectsBadFlagsBeforeFetching(t *testing.T) {
	now := time.Now()
	if _, err := (&planFlags{starredAfter: "yesterday"}).filter(now); err == nil {
		t.Error("a date that cannot be parsed was accepted")
	}
	if _, err := (&planFlags{exclude: []string{"[bad"}}).filter(now); err == nil {
		t.Error("a malformed glob was accepted")
	}
}

// Dormant stars are reported, never dropped: a repository that stopped being
// pushed to can be exactly the one worth keeping, and the flag says nothing
// about whether it belongs in a list.
func TestReportStale(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	repos := []gh.Repo{
		{FullName: "a/live", PushedAt: now.AddDate(0, -1, 0)},
		{FullName: "b/quiet", PushedAt: now.AddDate(-5, 0, 0)},
		{FullName: "c/quieter", PushedAt: now.AddDate(-9, 0, 0)},
		{FullName: "d/unknown"},
	}
	var out strings.Builder
	if n := reportStale(repos, now.AddDate(-2, 0, 0), &out); n != 2 {
		t.Errorf("reported %d stale stars, want 2", n)
	}
	if !strings.Contains(out.String(), "c/quieter") || !strings.Contains(out.String(), "b/quiet") {
		t.Errorf("the report does not name the dormant repositories:\n%s", out.String())
	}
	if strings.Contains(out.String(), "a/live") || strings.Contains(out.String(), "d/unknown") {
		t.Errorf("the report names a repository it should not:\n%s", out.String())
	}
	if i, j := strings.Index(out.String(), "c/quieter"), strings.Index(out.String(), "b/quiet"); i > j {
		t.Error("the report is not ordered oldest first")
	}

	out.Reset()
	if n := reportStale(repos, time.Time{}, &out); n != 0 || out.Len() != 0 {
		t.Errorf("an unset --stale-after reported %d stars and wrote %q", n, out.String())
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

// Every flag either changes the plan and is recorded in its settings, or does
// not and is named here as such. A flag in neither place is the bug this test
// exists to catch: a plan that cannot say what produced it.
func TestSettingsRecordEveryFlag(t *testing.T) {
	// Where the data comes from and where the plan goes, not what it says.
	unrecorded := map[string]string{
		"out":              "output path",
		"markdown":         "output path",
		"token":            "credential",
		"readme-workers":   "concurrency",
		"cache":            "cache path",
		"cache-ttl":        "cache freshness",
		"cache-full-ttl":   "cache freshness",
		"refresh":          "cache bypass",
		"readme-cache":     "cache path",
		"readme-cache-ttl": "cache freshness",
		"refresh-readmes":  "cache bypass",
		"stale-after":      "reports only; nothing is filtered",
		"verbose":          "terminal output",
		"show":             "terminal output",
		"incremental":      "recorded by extendPlan, not settings",
		"apply":            "what happens after the plan is written",
	}
	f := defaultPlanFlags(t)
	settings := f.settings()
	seen := map[string]bool{}
	for _, flag := range f.flags() {
		name := flag.Names()[0]
		seen[name] = true
		_, recorded := settings[name]
		_, listed := unrecorded[name]
		switch {
		case recorded && listed:
			t.Errorf("--%s is recorded in the settings but listed as not affecting the plan", name)
		case !recorded && !listed:
			t.Errorf("--%s is not recorded in the plan's settings; add it to settings(), or to this test's list if it cannot change the plan", name)
		}
	}
	for name := range unrecorded {
		if !seen[name] {
			t.Errorf("this test lists a --%s flag that no longer exists", name)
		}
	}
	for key := range settings {
		if !seen[key] {
			t.Errorf("settings() records %q, which is not a flag", key)
		}
	}
}

// Two plans built from different text must not record the same settings.
func TestSettingsRecordReadme(t *testing.T) {
	with := defaultPlanFlags(t)
	without := defaultPlanFlags(t)
	without.withReadme = false
	if reflect.DeepEqual(with.settings(), without.settings()) {
		t.Fatal("--readme=false and the default record identical settings")
	}
	if got := without.settings()["readme"]; got != "false" {
		t.Errorf("readme recorded as %q, want false", got)
	}
}

// An incremental run keeps the categories a previous run produced, so it must
// keep the settings that produced them too: the clustering flags on its own
// command line did nothing, and recording them would describe a plan that was
// never built. The pass itself is recorded separately, with the placement
// knobs that did apply.
func TestExtendPlanKeepsTheSettingsThatBuiltTheCategories(t *testing.T) {
	built := defaultPlanFlags(t)
	first, err := buildPlan(context.Background(), nil, "octocat", synthRepos(60), built)
	if err != nil {
		t.Fatal(err)
	}
	first.Stamp(time.Date(2026, 9, 1, 8, 30, 0, 0, time.UTC))

	extended := defaultPlanFlags(t)
	extended.algorithm = "kmeans"
	extended.maxK = 5
	extended.minCohesion = 0.9
	extended.seed = 42
	extended.minSimilarity = 0.11
	extended.outlierSigmas = 0
	extended.multiList = 2
	extended.multiRatio = 0.5
	second, err := extendPlan(context.Background(), nil, "octocat", synthRepos(66), extended, first)
	if err != nil {
		t.Fatal(err)
	}

	for key, want := range first.Settings {
		if got := second.Settings[key]; got != want {
			t.Errorf("settings[%q] = %q after the incremental run, want %q from the run that built the categories", key, got, want)
		}
	}
	if first.Settings["algorithm"] != "agglomerative" || second.Settings["algorithm"] == "kmeans" {
		t.Errorf("the incremental run's --algorithm kmeans was recorded over the previous run's %q", first.Settings["algorithm"])
	}
	want := map[string]string{
		"incremental":            "true",
		"incremental-from":       "2026-09-01T08:30:00Z",
		"place-min-similarity":   "0.11",
		"place-outlier-sigmas":   "0",
		"place-multi-list":       "2",
		"place-multi-list-ratio": "0.5",
	}
	for key, v := range want {
		if got := second.Settings[key]; got != v {
			t.Errorf("settings[%q] = %q, want %q", key, got, v)
		}
	}
	if _, ok := first.Settings["incremental"]; ok {
		t.Error("the previous plan was marked incremental in place; the settings must be copied, not shared")
	}

	// A plan written before settings were recorded has nothing to carry
	// forward, and extending it must still record the pass.
	first.Settings = nil
	third, err := extendPlan(context.Background(), nil, "octocat", synthRepos(66), extended, first)
	if err != nil {
		t.Fatal(err)
	}
	if third.Settings["incremental"] != "true" {
		t.Error("extending a plan without settings did not record the incremental pass")
	}
}
