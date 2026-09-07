// Command constellation groups your GitHub stars into categories with an
// unsupervised model and files them into GitHub star lists.
//
// Stars are read through the GitHub REST API; star lists are read and written
// through the GraphQL API, which needs a token carrying the "user" scope.
// Nothing is written until you ask for it with the apply command, or plan
// --apply.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/mrueg/constellation/internal/cluster"
	"github.com/mrueg/constellation/internal/embed"
	"github.com/mrueg/constellation/internal/gh"
	"github.com/mrueg/constellation/internal/plan"
	"github.com/mrueg/constellation/internal/textproc"
	"github.com/mrueg/constellation/internal/ui"
)

func main() {
	ui.Configure()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := command().Run(ctx, os.Args); err != nil {
		switch {
		case errors.Is(err, context.Canceled):
			fmt.Fprintln(os.Stderr, ui.Warn("\ninterrupted"))
			os.Exit(130)
		case errors.Is(err, gh.ErrNeedsUserScope):
			// Distinct exit code so a script can tell "grant the scope and
			// resume" apart from a genuine failure. Resuming is cheap:
			// applying is idempotent and skips what is already filed.
			fmt.Fprintln(os.Stderr, ui.Removed("error:"), err)
			fmt.Fprintln(os.Stderr, "any work already done is saved on GitHub; re-run the same command to continue.")
			os.Exit(3)
		default:
			fmt.Fprintln(os.Stderr, ui.Removed("error:"), err)
			os.Exit(1)
		}
	}
}

// envShow bounds how many categories are printed, for callers that cannot pass
// a flag — a recording, a wrapper script, a shell profile. The flag still wins
// when both are given.
const envShow = "CONSTELLATION_SHOW"

// command builds the CLI. Planning and applying are separate commands rather
// than one command with a flag, because only one of them changes anything.
func command() *cli.Command {
	return &cli.Command{
		Name:  "constellation",
		Usage: "categorize your GitHub stars into GitHub star lists",
		Description: `Reads your stars through the GitHub REST API, groups them with an
unsupervised model, and files them into star lists through the GraphQL API.

Planning never touches your account. Applying does, and needs a token carrying
the "user" scope:

   gh auth login             # reading needs no extra scope
   gh auth refresh -s user   # required before ` + "`constellation apply`" + `

` + "`constellation reset`" + ` deletes the lists this tool created, leaving lists you
made yourself alone.`,
		Version: buildVersion(),
		// Adds a "completion <bash|zsh|fish|pwsh>" subcommand. A packaged
		// install can source its output; there is nothing to maintain here
		// because the flags it completes come from the command tree.
		EnableShellCompletion: true,
		Commands:              []*cli.Command{planCommand(), applyCommand(), showCommand(), resetCommand()},
	}
}

func planCommand() *cli.Command {
	f, a := &planFlags{}, &applyFlags{}
	return &cli.Command{
		Name:  "plan",
		Usage: "group your stars and write a plan",
		Description: "Nothing is written to GitHub unless you pass --apply. The apply-side flags " +
			"below (--reconcile, --adopt, --private, --force, --limit, --only) take effect only then.",
		Flags:  append(f.flags(), a.flags()...),
		Action: func(ctx context.Context, _ *cli.Command) error { return runPlan(ctx, f, a) },
	}
}

func applyCommand() *cli.Command {
	a := &applyFlags{}
	flags := append([]cli.Flag{
		&cli.StringFlag{
			Name:        "plan",
			Value:       defaultPlanPath,
			Usage:       "plan to apply",
			Destination: &a.planPath,
		},
		// Registered here rather than in the shared set: "plan" already has
		// its own --show, and appending both produced a flag listed twice
		// with two different descriptions, the second unreachable.
		&cli.IntFlag{Name: "show", Usage: "print only the first N categories of the plan before applying (0 = all)", Sources: cli.EnvVars(envShow), Destination: &a.show},
		&cli.StringFlag{Name: "token", Usage: "GitHub token; defaults to $GITHUB_TOKEN, then $GH_TOKEN, then the gh CLI's stored token. Changing lists needs the 'user' scope. Visible to other users in the process list — prefer the environment", Destination: &a.token},
	}, a.flags()...)
	return &cli.Command{
		Name:        "apply",
		Usage:       "create the lists and file the stars",
		Description: "Changes your account. Asks for confirmation unless -y is given.",
		Flags:       flags,
		Action: func(ctx context.Context, _ *cli.Command) error {
			p, err := plan.Load(a.planPath)
			if err != nil {
				return err
			}
			p.PrintN(os.Stdout, false, a.show)
			return applyPlan(ctx, p, a)
		},
	}
}

func showCommand() *cli.Command {
	var token string
	return &cli.Command{
		Name:  "show",
		Usage: "show the star lists you already have",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "token", Usage: "GitHub token; defaults to $GITHUB_TOKEN, then $GH_TOKEN, then the gh CLI's stored token. Changing lists needs the 'user' scope", Destination: &token},
		},
		Action: func(ctx context.Context, _ *cli.Command) error { return runShow(ctx, token) },
	}
}

func resetCommand() *cli.Command {
	var (
		token  string
		yes    bool
		dryRun bool
		keepGo bool
		delay  time.Duration
	)
	return &cli.Command{
		Name:  "reset",
		Usage: "delete the star lists this tool created, leaving hand-made lists alone",
		Description: "Deletes lists permanently; the repositories stay starred. Identifies its own " +
			"lists by the marker their descriptions carry, so lists you made yourself are never " +
			"touched. Run with --dry-run first.",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "dry-run", Aliases: []string{"n"}, Usage: "list what would be deleted without deleting anything", Destination: &dryRun},
			&cli.BoolFlag{Name: "yes", Aliases: []string{"y"}, Usage: "do not ask for confirmation", Destination: &yes},
			&cli.BoolFlag{Name: "continue", Usage: "keep going past a list that fails to delete", Destination: &keepGo},
			&cli.DurationFlag{Name: "delay", Value: time.Second, Usage: "pause between deletions; GitHub asks for at least a second between mutating requests", Destination: &delay},
			&cli.StringFlag{Name: "token", Usage: "GitHub token; defaults to $GITHUB_TOKEN, then $GH_TOKEN, then the gh CLI's stored token. Changing lists needs the 'user' scope", Destination: &token},
		},
		Action: func(ctx context.Context, _ *cli.Command) error {
			return runReset(ctx, token, yes, dryRun, keepGo, delay)
		},
	}
}

const defaultPlanPath = "constellation-plan.json"

// Build information, set by the linker at release time. The defaults are what
// a plain `go build` produces.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func buildVersion() string {
	if version == "dev" {
		return version
	}
	return fmt.Sprintf("%s (%s, built %s)", version, commit, date)
}

type planFlags struct {
	out      string
	markdown string
	token    string

	lsaDims       int
	lsaPower      int
	lsaOversample int
	lsaSeed       int64
	withReadme    bool
	readmeBytes   int
	readmeWords   int
	readmeWeight  float64
	langWeight    float64
	topicWeight   float64
	topicWordW    float64
	topicInferred float64
	topicMinCount int
	weighting     string
	bm25K1        float64
	bm25B         float64
	readmeWorker  int

	clusters      int
	minK, maxK    int
	minSize       int
	maxLists      int
	minSimilarity float64
	minCohesion   float64
	splitParts    int
	splitMinSize  int
	splitMaxSize  int
	rescueK       int
	rescueAgree   float64
	mergeOverlap  float64
	minCoverage   float64
	consensus     int
	algorithm     string
	outlierSigmas float64
	multiList     int
	multiRatio    float64
	restarts      int
	seed          int64
	sample        int
	minDF         int
	maxDFRatio    float64
	maxVocab      int

	cachePath       string
	cacheMaxAge     time.Duration
	refresh         bool
	readmeCachePath string
	readmeMaxAge    time.Duration
	refreshReadmes  bool
	includeForks    bool
	includeArchived bool
	verbose         bool
	show            int
	apply           bool
}

func (f *planFlags) flags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Name: "out", Value: defaultPlanPath, Usage: "where to write the plan", Destination: &f.out},
		&cli.StringFlag{Name: "markdown", Usage: "also write the categories as a markdown index to this file", Destination: &f.markdown},
		&cli.StringFlag{Name: "token", Usage: "GitHub token; defaults to $GITHUB_TOKEN, then $GH_TOKEN, then the gh CLI's stored token. Changing lists needs the 'user' scope. Visible to other users in the process list — prefer the environment", Destination: &f.token},

		&cli.IntFlag{Name: "lsa-dims", Value: 150, Usage: "latent dimensions kept by the SVD", Destination: &f.lsaDims},
		&cli.Int64Flag{Name: "lsa-seed", Value: 1, Usage: "seed for the randomized SVD, deliberately separate from --seed so that changing the clustering seed does not also change the embedding underneath it", Destination: &f.lsaSeed},
		&cli.IntFlag{Name: "lsa-oversample", Usage: "extra columns in the random sketch; 0 scales it with --lsa-dims. Too few leaves the tail of the factorization under-converged, which shows up as the answer depending on --lsa-seed", Destination: &f.lsaOversample},
		&cli.IntFlag{Name: "lsa-power", Value: 4, Usage: "power iterations for the randomized SVD; more is slower and sharper", Destination: &f.lsaPower},
		&cli.BoolFlag{Name: "readme", Value: true, Usage: "read each repository's README as well as its description; --readme=false to skip", Destination: &f.withReadme},
		&cli.IntFlag{Name: "readme-bytes", Value: 8192, Usage: "how much of each README to fetch", Destination: &f.readmeBytes},
		&cli.Float64Flag{Name: "topic-weight", Value: textproc.WeightTopic, Usage: "how much a repository topic counts, kept whole; 0 falls back to splitting it into words", Destination: &f.topicWeight},
		&cli.Float64Flag{Name: "topic-word-weight", Value: textproc.WeightTopicWord, Usage: "how much each word of a multi-word topic counts on its own", Destination: &f.topicWordW},
		&cli.Float64Flag{Name: "topic-inferred-weight", Value: 0.5, Usage: "scales how much topics recognized in an untagged repository's name, description and README opening count, relative to each other; 0 disables the inference", Destination: &f.topicInferred},
		&cli.IntFlag{Name: "topic-min-count", Value: 3, Usage: "how often a topic must be used in your stars before it can be inferred from a description", Destination: &f.topicMinCount},
		&cli.Float64Flag{Name: "language-weight", Value: textproc.WeightLanguage, Usage: "how much a repository's programming language counts; every repository in a language shares this exact term, so it groups strongly", Destination: &f.langWeight},
		&cli.Float64Flag{Name: "readme-weight", Value: textproc.WeightReadme, Usage: "how much each README term counts, against 3 for a topic and 1 for a description word", Destination: &f.readmeWeight},
		&cli.IntFlag{Name: "readme-words", Value: 120, Usage: "words of README prose to keep; the opening is a project's own summary, the rest is installation notes", Destination: &f.readmeWords},
		&cli.IntFlag{Name: "readme-workers", Value: 8, Usage: "concurrent README fetches", Destination: &f.readmeWorker},

		&cli.IntFlag{Name: "multi-list", Value: 1, Usage: "most lists one repository may join; 1 keeps each repository in a single list", Destination: &f.multiList},
		&cli.Float64Flag{Name: "multi-list-ratio", Value: 0.8, Usage: "how close a further category must fit, as a fraction of the best fit, before a repository joins it too", Destination: &f.multiRatio},
		&cli.IntFlag{Name: "clusters", Usage: "number of categories; 0 means cut the tree at --max-clusters, or search by silhouette under --algorithm kmeans", Destination: &f.clusters},
		&cli.IntFlag{Name: "min-clusters", Value: 6, Usage: "smallest number of categories to consider when choosing automatically (--algorithm kmeans only)", Destination: &f.minK},
		&cli.IntFlag{Name: "max-clusters", Value: gh.MaxLists, Usage: "most categories to produce; where the dendrogram is cut under the default algorithm", Destination: &f.maxK},
		&cli.IntFlag{Name: "min-size", Value: 4, Usage: "dissolve categories with fewer repositories than this", Destination: &f.minSize},
		&cli.IntFlag{Name: "max-lists", Value: gh.MaxLists, Usage: "keep at most this many categories; GitHub allows 32 star lists per account", Destination: &f.maxLists},
		&cli.Float64Flag{Name: "min-name-coverage", Value: 0.15, Usage: "dissolve a category when fewer than this share of its members carry the term it is named after; 0 keeps every category", Destination: &f.minCoverage},
		&cli.Float64Flag{Name: "merge-duplicates", Value: 0.3, Usage: "merge two categories sharing at least this fraction of their describing terms; 0 disables it", Destination: &f.mergeOverlap},
		&cli.IntFlag{Name: "split-parts", Value: 3, Usage: "most pieces an incoherent category is divided into before being dissolved; 0 dissolves it outright", Destination: &f.splitParts},
		&cli.IntFlag{Name: "split-max-size", Value: 200, Usage: "divide a category larger than this even when it holds together; 0 leaves large categories alone", Destination: &f.splitMaxSize},
		&cli.IntFlag{Name: "split-min-size", Value: 40, Usage: "smallest category worth trying to divide", Destination: &f.splitMinSize},
		&cli.IntFlag{Name: "rescue-neighbours", Value: 10, Usage: "nearest categorized neighbours an uncategorized repository consults before joining them; 0 disables it", Destination: &f.rescueK},
		&cli.Float64Flag{Name: "rescue-agreement", Value: 0.8, Usage: "share of those neighbours that must agree", Destination: &f.rescueAgree},
		&cli.Float64Flag{Name: "min-cohesion", Value: 0.36, Usage: "dissolve a category whose members do not on average resemble it this much; 0 keeps every cluster", Destination: &f.minCohesion},
		&cli.Float64Flag{Name: "min-similarity", Value: 0.05, Usage: "absolute floor on how well a repository must fit its category", Destination: &f.minSimilarity},
		&cli.Float64Flag{Name: "outlier-sigmas", Value: 3, Usage: "leave a repository uncategorized when its fit falls this many robust deviations below its category's typical fit (0 disables)", Destination: &f.outlierSigmas},
		&cli.StringFlag{Name: "algorithm", Value: "agglomerative", Usage: "clustering algorithm: kmeans, or agglomerative (deterministic — no seed, same answer every time)", Destination: &f.algorithm},
		&cli.IntFlag{Name: "consensus", Value: 1, Usage: "cluster the agreement between this many runs instead of trusting one; 1 disables it (--algorithm kmeans only)", Destination: &f.consensus},
		&cli.IntFlag{Name: "restarts", Value: 4, Usage: "restarts per candidate k (--algorithm kmeans only)", Destination: &f.restarts},
		&cli.Int64Flag{Name: "seed", Value: 1, Usage: "random seed (--algorithm kmeans only; the default clustering ignores it — see --lsa-seed, which the default path does use)", Destination: &f.seed},
		&cli.IntFlag{Name: "silhouette-sample", Value: 600, Usage: "repositories sampled when scoring a candidate k (--algorithm kmeans only)", Destination: &f.sample},
		&cli.StringFlag{Name: "weighting", Value: "tfidf", Usage: "term weighting: tfidf or bm25 (saturating, length-corrected)", Destination: &f.weighting},
		&cli.Float64Flag{Name: "bm25-k1", Value: 1.2, Usage: "term-frequency saturation for --weighting bm25", Destination: &f.bm25K1},
		&cli.Float64Flag{Name: "bm25-b", Value: 0.75, Usage: "length correction for --weighting bm25", Destination: &f.bm25B},
		&cli.IntFlag{Name: "min-df", Value: 2, Usage: "ignore terms appearing in fewer repositories than this", Destination: &f.minDF},
		&cli.Float64Flag{Name: "max-df", Value: 0.4, Usage: "ignore terms appearing in more than this fraction of repositories", Destination: &f.maxDFRatio},
		&cli.IntFlag{Name: "max-vocab", Value: 12000, Usage: "cap on the vocabulary the model is built from", Destination: &f.maxVocab},

		&cli.StringFlag{Name: "cache", Value: gh.DefaultCachePath(), Usage: "where to cache your stars", Destination: &f.cachePath},
		&cli.DurationFlag{Name: "cache-ttl", Value: 24 * time.Hour, Usage: "how long a cached star list stays usable", Destination: &f.cacheMaxAge},
		&cli.BoolFlag{Name: "refresh", Usage: "ignore the star cache and re-fetch the star list; cached READMEs are kept, see --refresh-readmes", Destination: &f.refresh},
		&cli.StringFlag{Name: "readme-cache", Value: gh.DefaultReadmeCachePath(), Usage: "where to cache the README openings", Destination: &f.readmeCachePath},
		&cli.DurationFlag{Name: "readme-cache-ttl", Value: 30 * 24 * time.Hour, Usage: "how long a cached README stays usable; 0 keeps it forever", Destination: &f.readmeMaxAge},
		&cli.BoolFlag{Name: "refresh-readmes", Usage: "ignore the README cache and read every README again", Destination: &f.refreshReadmes},
		&cli.BoolFlag{Name: "include-forks", Usage: "include starred forks", Destination: &f.includeForks},
		&cli.BoolFlag{Name: "include-archived", Usage: "include starred archived repositories", Destination: &f.includeArchived},
		&cli.BoolFlag{Name: "verbose", Aliases: []string{"v"}, Usage: "list every repository in every category", Destination: &f.verbose},
		&cli.IntFlag{Name: "show", Usage: "print only the first N categories (0 = all); the plan file always holds every one", Sources: cli.EnvVars(envShow), Destination: &f.show},
		&cli.BoolFlag{Name: "apply", Usage: "apply the plan straight away instead of only writing it", Destination: &f.apply},
	}
}

func runPlan(ctx context.Context, f *planFlags, af *applyFlags) error {
	client, err := gh.NewClient(f.token)
	if err != nil {
		return err
	}
	client.Log = progress
	user, err := client.Viewer(ctx)
	if err != nil {
		return fmt.Errorf("checking the token: %w", err)
	}

	all, err := loadStars(ctx, client, user, f)
	if err != nil {
		return err
	}
	repos := filterRepos(all, f)
	if len(repos) == 0 {
		return fmt.Errorf("no stars to categorize")
	}
	if f.withReadme {
		readmes := loadReadmes(f)
		applyReadmes(readmes, repos)
		fetchReadmes(ctx, client, repos, f)
		// Saved even on an interrupt, so a Ctrl-C partway through does not
		// throw away the READMEs already fetched, and even when every fetch
		// failed, because a README that cannot be read is marked as such and
		// that verdict is worth keeping too.
		if harvestReadmes(readmes, repos, time.Now()) > 0 {
			if err := gh.SaveReadmes(f.readmeCachePath, readmes); err != nil {
				progress("could not update the README cache: %v", err)
			}
		}
		// An interrupt during fetching means stop, not "build a plan from
		// whatever was fetched so far". The cache above is preserved; the plan
		// is not written from partial input.
		if err := ctx.Err(); err != nil {
			return err
		}
	}

	// Reading existing lists is a convenience, not a requirement: without a
	// usable token the plan is still correct, it just cannot mark which
	// categories already exist.
	lists, err := gh.NewListsClient(f.token)
	if err != nil {
		progress("not reading existing lists: %v", err)
		lists = nil
	}
	p, err := buildPlan(ctx, lists, user, repos, f)
	if err != nil {
		return err
	}
	p.Stamp(time.Now())

	p.PrintN(os.Stdout, f.verbose, f.show)
	if err := p.Save(f.out); err != nil {
		return err
	}
	fmt.Printf("plan written to %s\n", f.out)
	if f.markdown != "" {
		if err := writeMarkdown(p, f.markdown); err != nil {
			return err
		}
		fmt.Printf("markdown index written to %s\n", f.markdown)
	}

	// plan --apply shares one --token; without this the write half would fall
	// back to the ambient credential and could act as a different account.
	if af.token == "" {
		af.token = f.token
	}
	if !f.apply {
		fmt.Printf("\nnothing has been changed on GitHub. Review the plan, then run:\n  constellation apply --plan %s\n", f.out)
		return nil
	}
	return applyPlan(ctx, p, af)
}

// writeMarkdown renders the plan to a file. The rendering itself ignores write
// errors, as terminal output does, so the buffer is flushed explicitly to
// catch a full disk rather than leaving a truncated index behind.
func writeMarkdown(p *plan.Plan, path string) (err error) {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := file.Close(); err == nil {
			err = cerr
		}
	}()
	buf := bufio.NewWriter(file)
	p.Markdown(buf)
	return buf.Flush()
}

// buildPlan runs the model: embed the repositories, cluster them, name the
// clusters, and reconcile against the lists that already exist.
func buildPlan(ctx context.Context, lists *gh.ListsClient, user string, repos []gh.Repo, f *planFlags) (*plan.Plan, error) {
	// The vocabulary of topics the collection already uses, so that a
	// repository nobody labelled can be matched against its neighbours'.
	var topicLists [][]string
	for _, r := range repos {
		topicLists = append(topicLists, r.Topics)
	}
	index := textproc.NewTopicIndex(topicLists, f.topicMinCount)
	if f.topicInferred <= 0 {
		index = nil
	}

	docs := make([]embed.Doc, len(repos))
	for i, r := range repos {
		// --readme=false has to exclude the text as well as the fetch, or a
		// cached README from an earlier run silently stays in the model.
		// Truncation is applied here rather than only at fetch time, so that
		// changing --readme-words takes effect against an existing cache
		// instead of silently doing nothing.
		readme := ""
		if f.withReadme {
			readme = firstWords(r.Readme, f.readmeWords)
		}
		docs[i] = textproc.BuildWith(weights(f), index, r.Owner(), r.Name(), r.Description, r.Language, r.Topics, readme)
	}

	emb := embedder(f)
	progress("embedding %d repositories with %s", len(repos), emb.Name())
	sp, err := emb.Embed(ctx, docs)
	if err != nil {
		return nil, err
	}
	if sp.Dim == 0 {
		// --min-df 1 alone is a trap: terms seen in exactly one repository
		// outnumber the vocabulary cap, and since the cap keeps the rarest
		// terms the whole vocabulary becomes words that by definition group
		// nothing. Lifting the cap at the same time is what actually works.
		return nil, fmt.Errorf("the corpus produced an empty vocabulary; try --min-df 1 --max-vocab 0")
	}

	switch {
	case f.algorithm == "agglomerative":
		progress("clustering %d repositories over %d dimensions by Ward linkage", len(repos), sp.Dim)
	case f.consensus > 1:
		progress("clustering %d repositories over %d dimensions, %d runs in consensus", len(repos), sp.Dim, f.consensus)
	default:
		progress("clustering %d repositories over %d dimensions", len(repos), sp.Dim)
	}
	base := cluster.Options{
		K: f.clusters, MinK: f.minK, MaxK: f.maxK,
		Restarts: f.restarts, MaxIter: 60, Seed: f.seed,
		SilhouetteSample: f.sample,
	}
	var res *cluster.Result
	switch f.algorithm {
	case "kmeans":
		res = cluster.Consensus(sp, base, f.consensus)
	case "agglomerative":
		// Nothing here is seeded, so the cluster count cannot be chosen by the
		// silhouette sweep either; the tree is cut at the list cap and the
		// coherence floor prunes whatever does not deserve to be a category.
		k := f.clusters
		if k <= 0 {
			k = f.maxK
		}
		res = cluster.AgglomerateContext(ctx, sp, k)
	default:
		return nil, fmt.Errorf("unknown --algorithm %q (want kmeans or agglomerative)", f.algorithm)
	}
	// Clustering is the long pure-CPU stretch. If it was interrupted, stop here
	// rather than refining and naming a partial result.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	res = cluster.Refine(sp, res, cluster.RefineOptions{
		MinSize: f.minSize, MaxLists: f.maxLists, MinCohesion: f.minCohesion,
		SplitParts: f.splitParts, SplitMinSize: f.splitMinSize,
		RescueNeighbours: f.rescueK, RescueAgreement: f.rescueAgree,
		SplitMaxSize:  f.splitMaxSize,
		MinSimilarity: f.minSimilarity, OutlierSigmas: f.outlierSigmas,
	})
	if res.K == 0 {
		return nil, fmt.Errorf("no category survived the --min-size %d filter; lower it or raise --max-clusters", f.minSize)
	}
	// Runs after refinement so a repository can only ever be added to a
	// category that survived it.
	// After refinement, so that a merge cannot resurrect a category the
	// coherence floor already dissolved.
	res = cluster.MergeDuplicates(docs, sp, res, f.mergeOverlap)
	res = cluster.DropUnnamed(docs, sp, res, f.minCoverage)
	res = cluster.Soften(sp, res, f.multiRatio, f.multiList)
	labels := cluster.Names(docs, res.Assign, res.K, 6)

	// Existing lists are read so the plan can tell new categories from ones
	// already on the account. Failing to read them is not fatal: the plan is
	// still valid, it just cannot mark which lists already exist.
	var existing []gh.List
	if lists != nil {
		if ls, err := lists.Lists(ctx); err == nil {
			existing = ls
		} else {
			progress("could not read existing lists: %v", err)
		}
	}
	p := plan.Build(user, repos, sp, res, labels, existing)
	p.Settings = f.settings()

	// Existing lists spend the same budget as new ones, so a plan that fits on
	// its own can still be too big for the account.
	if fresh := p.NewLists(); len(existing)+fresh > gh.MaxLists {
		progress("warning: GitHub allows %d star lists and you already have %d, but this plan adds %d new ones; "+
			"re-run with --max-clusters %d to fit", gh.MaxLists, len(existing), fresh, gh.MaxLists-len(existing))
	}
	return p, nil
}

// settings records the tuning that produced a plan. Only the knobs that change
// the categories are kept: cache paths, worker counts and output paths say
// nothing about why the plan looks the way it does.
func (f *planFlags) settings() map[string]string {
	num := func(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }
	return map[string]string{
		"algorithm":             f.algorithm,
		"weighting":             f.weighting,
		"lsa-dims":              strconv.Itoa(f.lsaDims),
		"lsa-seed":              strconv.FormatInt(f.lsaSeed, 10),
		"min-df":                strconv.Itoa(f.minDF),
		"max-df":                num(f.maxDFRatio),
		"max-vocab":             strconv.Itoa(f.maxVocab),
		"max-clusters":          strconv.Itoa(f.maxK),
		"min-clusters":          strconv.Itoa(f.minK),
		"clusters":              strconv.Itoa(f.clusters),
		"min-size":              strconv.Itoa(f.minSize),
		"min-cohesion":          num(f.minCohesion),
		"min-name-coverage":     num(f.minCoverage),
		"merge-duplicates":      num(f.mergeOverlap),
		"split-parts":           strconv.Itoa(f.splitParts),
		"rescue-neighbours":     strconv.Itoa(f.rescueK),
		"rescue-agreement":      num(f.rescueAgree),
		"multi-list":            strconv.Itoa(f.multiList),
		"language-weight":       num(f.langWeight),
		"topic-weight":          num(f.topicWeight),
		"topic-word-weight":     num(f.topicWordW),
		"topic-inferred-weight": num(f.topicInferred),
		"readme-weight":         num(f.readmeWeight),
		"readme-words":          strconv.Itoa(f.readmeWords),
		"consensus":             strconv.Itoa(f.consensus),
	}
}

func weights(f *planFlags) textproc.Weights {
	w := textproc.DefaultWeights()
	w.Readme = f.readmeWeight
	w.Language = f.langWeight
	w.Topic = f.topicWeight
	w.TopicWord = f.topicWordW
	// One knob scales all three sources, keeping their measured ratio.
	w.TopicFromName = textproc.WeightTopicFromName * f.topicInferred
	w.TopicFromText = textproc.WeightTopicFromText * f.topicInferred
	w.TopicFromReadme = textproc.WeightTopicFromReadme * f.topicInferred
	return w
}

// tfidfBase is the weighted term matrix that the LSA backend factors.
func tfidfBase(f *planFlags) *embed.TFIDF {
	return &embed.TFIDF{
		MinDF: f.minDF, MaxDFRatio: f.maxDFRatio, MaxVocab: f.maxVocab,
		BM25: f.weighting == "bm25", K1: f.bm25K1, B: f.bm25B,
	}
}

// oversample picks the sketch width: whatever was asked for, or half the
// dimensions when it was not.
func oversample(f *planFlags) int {
	if f.lsaOversample > 0 {
		return f.lsaOversample
	}
	return max(10, f.lsaDims/2)
}

// embedder builds the model: a weighted term matrix reduced by a truncated
// SVD. There is one, so there is nothing to choose between.
func embedder(f *planFlags) embed.Embedder {
	// The sketch is widened in proportion to the number of dimensions asked
	// for. A fixed oversample of ten is the textbook figure for a small number
	// of dimensions over a fast-decaying spectrum; this one decays slowly, and
	// at 150 dimensions a sketch of 160 left the tail of the factorization
	// badly under-converged. The visible effect was that the answer depended
	// on the seed: three seeds placed 3663, 3398 and 3112 of the same 4169
	// stars.
	//
	// A fixed seed always reproduced its own answer, so this never showed up
	// as flakiness — it showed up as the seed being a hidden tuning knob with
	// more influence than any documented one.
	return &embed.LSA{
		Base: tfidfBase(f), Dims: f.lsaDims,
		Oversample: oversample(f),
		Power:      f.lsaPower, Seed: f.lsaSeed,
	}
}

func loadStars(ctx context.Context, c *gh.Client, user string, f *planFlags) ([]gh.Repo, error) {
	if !f.refresh {
		repos, at, err := gh.LoadCache(f.cachePath, user, f.cacheMaxAge)
		if err != nil {
			return nil, err
		}
		if len(repos) > 0 {
			progress("using %d cached stars from %s (--refresh to re-fetch)", len(repos), at.Format(time.RFC822))
			return repos, nil
		}
	}
	repos, err := c.Starred(ctx)
	if err != nil {
		return nil, err
	}
	if err := gh.SaveCache(f.cachePath, user, repos); err != nil {
		progress("could not write cache: %v", err)
	}
	return repos, nil
}

func filterRepos(repos []gh.Repo, f *planFlags) []gh.Repo {
	out := repos[:0:0]
	for _, r := range repos {
		if r.Fork && !f.includeForks {
			continue
		}
		if r.Archived && !f.includeArchived {
			continue
		}
		out = append(out, r)
	}
	if n := len(repos) - len(out); n > 0 {
		progress("skipped %d forked or archived stars", n)
	}
	return out
}

// fetchReadmes fills in the READMEs that are not already cached.
//
// A repository description is nine words at the median and a third of
// repositories carry no topics at all, so without this the model is working
// from almost nothing. Only the opening of each README is kept: a project's
// first paragraph is its own summary of itself, while the rest is installation
// instructions and contribution guidelines that every repository shares and
// that would therefore group them by nothing.
//
// Results are cached with the stars, so the cost is paid once rather than on
// every re-run.
// readmeFetcher is the one method fetchReadmes needs. Taking an interface
// rather than the concrete client means the tests can supply a stand-in
// without the package exporting a constructor that exists only for them.
type readmeFetcher interface {
	Readme(ctx context.Context, owner, repo string, limit int) (string, error)
}

func fetchReadmes(ctx context.Context, c readmeFetcher, repos []gh.Repo, f *planFlags) int {
	workers := f.readmeWorker
	if workers < 1 {
		workers = 1
	}
	var todo []int
	for i, r := range repos {
		if r.Readme == "" {
			todo = append(todo, i)
		}
	}
	if len(todo) == 0 {
		return 0
	}
	progress("fetching %d READMEs (cached afterwards; --readme=false to skip)", len(todo))

	var (
		mu      sync.Mutex
		fetched int
		failed  int
		lastErr error
	)
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				text, err := c.Readme(ctx, repos[i].Owner(), repos[i].Name(), f.readmeBytes)
				if err != nil {
					mu.Lock()
					failed++
					lastErr = err
					// A README that cannot be read now will not become
					// readable later: over a megabyte, blocked, or taken
					// down. Marking it stops every future run spending a
					// request rediscovering that. An interruption is
					// different — that repository must stay on the list.
					if ctx.Err() == nil {
						repos[i].Readme = " "
					}
					mu.Unlock()
					continue
				}
				// An empty README is a real answer, and marking it stops the
				// next run asking again.
				readme := firstWords(text, f.readmeWords)
				if readme == "" {
					readme = " "
				}
				mu.Lock()
				repos[i].Readme = readme
				fetched++
				mu.Unlock()
			}
		}()
	}
	for _, i := range todo {
		select {
		case jobs <- i:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			mu.Lock()
			done := fetched
			mu.Unlock()
			return done
		}
	}
	close(jobs)
	wg.Wait()

	// Silently dropping these was hiding a bad token or an exhausted rate
	// limit behind quietly worse categories.
	if failed > 0 {
		progress("warning: %d of %d READMEs could not be read (last error: %v)", failed, len(todo), lastErr)
	}
	return fetched
}

// loadReadmes reads the README cache, migrating out of the star cache the
// first time it runs after the two were split. Failing to read it is not
// fatal: the run re-fetches instead.
func loadReadmes(f *planFlags) gh.ReadmeCache {
	if f.refreshReadmes {
		return gh.ReadmeCache{}
	}
	_, statErr := os.Stat(f.readmeCachePath)
	c, err := gh.LoadReadmes(f.readmeCachePath, f.readmeMaxAge)
	if err != nil {
		progress("not using the README cache: %v", err)
		c = gh.ReadmeCache{}
	}
	// Only when there is no README cache at all: an existing one that has
	// simply expired must not be refilled from a star cache whose copies are
	// older still.
	if os.IsNotExist(statErr) {
		if migrated := gh.ReadmesFromStarCache(f.cachePath); len(migrated) > 0 {
			migrated.Prune(f.readmeMaxAge)
			progress("carried %d READMEs over from the star cache", len(migrated))
			c = migrated
		}
	}
	if len(c) > 0 {
		progress("using %d cached READMEs (--refresh-readmes to re-read them)", len(c))
	}
	return c
}

// applyReadmes fills in the READMEs already cached, leaving only the missing
// ones to cost an API call.
func applyReadmes(c gh.ReadmeCache, repos []gh.Repo) {
	for i := range repos {
		if e, ok := c[repos[i].FullName]; ok {
			repos[i].Readme = e.Text
		}
	}
}

// harvestReadmes copies newly read READMEs into the cache and reports how many
// entries changed, which is what decides whether the cache is worth writing.
func harvestReadmes(c gh.ReadmeCache, repos []gh.Repo, now time.Time) int {
	changed := 0
	for _, r := range repos {
		if r.Readme == "" {
			continue
		}
		if e, ok := c[r.FullName]; ok && e.Text == r.Readme {
			continue
		}
		c[r.FullName] = gh.CachedReadme{Text: r.Readme, FetchedAt: now}
		changed++
	}
	return changed
}

// firstWords keeps the opening n words of a text.
func firstWords(s string, n int) string {
	if n <= 0 {
		return s
	}
	fields := strings.Fields(s)
	if len(fields) > n {
		fields = fields[:n]
	}
	return strings.Join(fields, " ")
}

type applyFlags struct {
	planPath  string
	token     string
	delay     time.Duration
	limit     int
	only      []string
	keepGo    bool
	reconcile bool
	dryRun    bool
	private   bool
	adopt     bool
	force     bool
	batch     int
	show      int
	verify    bool
	yes       bool
}

// flags are the ones shared by "apply" and by "plan --apply".
func (a *applyFlags) flags() []cli.Flag {
	base := []cli.Flag{
		&cli.DurationFlag{Name: "delay", Value: time.Second, Usage: "pause between writes; GitHub asks for at least a second between mutating requests", Destination: &a.delay},
		&cli.IntFlag{Name: "limit", Usage: "stop after this many repositories (0 = all); useful for a cautious first run", Destination: &a.limit},
		&cli.StringSliceFlag{Name: "only", Usage: "category names to apply; repeat the flag or separate with commas", Destination: &a.only},
		&cli.BoolFlag{Name: "dry-run", Aliases: []string{"n"}, Usage: "report what would change without changing anything; the only way to preview a --reconcile run, which deletes lists", Destination: &a.dryRun},
		&cli.IntFlag{Name: "batch", Value: 25, Usage: "membership changes per request; GraphQL runs them serially so the result is identical to sending them one at a time. A request that GitHub judges too complex is split and retried automatically, so this is a target rather than a limit", Destination: &a.batch},
		&cli.BoolFlag{Name: "force", Usage: "if the plan does not fit in GitHub's 32-list cap, delete lists this tool created that the plan no longer contains, smallest first, until it does; lists you made yourself are never touched", Destination: &a.force},
		&cli.BoolFlag{Name: "adopt", Usage: "take over an existing list whose name matches a category even if this tool did not create it. Its description is replaced by the plan's, which also brings it under the control of --reconcile and reset. Without this such a list is left untouched", Destination: &a.adopt},
		&cli.BoolFlag{Name: "private", Usage: "create the lists private instead of public; visibility is enforced on every run, so this also flips lists an earlier run made public, and clearing it makes them public again", Destination: &a.private},
		&cli.BoolFlag{Name: "reconcile", Usage: "make the account match the plan instead of only adding to it: repositories leave categories they no longer belong to and dropped categories are deleted, touching only lists this tool created. With --only it stays inside the named categories and deletes nothing", Destination: &a.reconcile},
		&cli.BoolFlag{Name: "continue", Usage: "keep going when a repository fails instead of stopping", Destination: &a.keepGo},
		&cli.BoolFlag{Name: "yes", Aliases: []string{"y"}, Usage: "do not ask for confirmation", Destination: &a.yes},
		&cli.BoolFlag{Name: "verify", Usage: "after applying, read the lists back and report any placement that did not land", Destination: &a.verify},
	}
	return base
}

// categories flattens the -only values, accepting both a repeated flag and a
// single comma-separated list.
func (a *applyFlags) categories() []string {
	var out []string
	for _, v := range a.only {
		for _, part := range strings.Split(v, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

func applyPlan(ctx context.Context, p *plan.Plan, a *applyFlags) error {
	c, err := gh.NewListsClient(a.token)
	if err != nil {
		return err
	}
	c.Log = progress
	if err := checkTokenOwner(ctx, c, p.User); err != nil {
		return err
	}

	only := a.categories()
	if !a.yes && !a.dryRun {
		if !stdinIsTerminal() {
			// Without a terminal the prompt would read EOF and count as "no",
			// so apply would exit having changed nothing with no hint why.
			// Say what to do instead.
			return fmt.Errorf("apply needs confirmation but stdin is not a terminal; " +
				"re-run with --yes to proceed without prompting, or --dry-run to preview")
		}
		ok, err := confirm(p, only, a.limit, a.reconcile, a.force)
		if err != nil {
			return err
		}
		if !ok {
			fmt.Println(ui.Warn("aborted; nothing was changed"))
			return nil
		}
	}

	res, err := plan.Apply(ctx, c, p, plan.ApplyOptions{
		Delay: a.delay, Limit: a.limit, Only: only,
		ContinueOnError: a.keepGo, Reconcile: a.reconcile, DryRun: a.dryRun,
		Private: a.private, Adopt: a.adopt, Force: a.force, Batch: a.batch, Out: os.Stdout,
	})
	if res != nil {
		if a.dryRun {
			fmt.Println(ui.Info("\ndry run: nothing was changed"))
		}
		fmt.Printf("\n%d %s %s created, %d %s %s filed, %d already in place\n",
			len(res.ListsCreated), plural(len(res.ListsCreated), "list", "lists"),
			pastTense(len(res.ListsCreated), a.dryRun),
			res.ReposFiled, plural(res.ReposFiled, "repository", "repositories"),
			pastTense(res.ReposFiled, a.dryRun), res.ReposSkipped)
		if len(res.ListsDeleted) > 0 || res.ReposRemoved > 0 {
			fmt.Printf("%d %s %s deleted, %d %s no longer in the plan %s removed\n",
				len(res.ListsDeleted), plural(len(res.ListsDeleted), "list", "lists"),
				pastTense(len(res.ListsDeleted), a.dryRun),
				res.ReposRemoved, plural(res.ReposRemoved, "placement", "placements"),
				pastTense(res.ReposRemoved, a.dryRun))
		}
		if res.StoppedAtCap {
			fmt.Printf("stopped at --limit %d; re-run without it to finish\n", a.limit)
		}
		if len(res.Adopted) > 0 {
			fmt.Printf("%d %s adopted: %s — now managed here, so --reconcile and reset can delete them\n",
				len(res.Adopted), plural(len(res.Adopted), "list", "lists"), strings.Join(res.Adopted, ", "))
		}
		if len(res.Conflicts) > 0 {
			fmt.Printf("%d categories skipped because a list of the same name was not created here (%s); --adopt to take them over\n",
				len(res.Conflicts), strings.Join(res.Conflicts, ", "))
		}
		for _, e := range res.Errors {
			fmt.Fprintln(os.Stderr, " ", ui.Removed("!"), e)
		}
	}
	if err != nil {
		return err
	}
	if a.verify && !a.dryRun {
		fmt.Println("\nverifying...")
		v, verr := plan.Verify(ctx, c, p, only, os.Stdout)
		if verr != nil {
			return verr
		}
		if v.OK() {
			fmt.Printf("%s all %d placements are in place\n", ui.Added("verified:"), v.Checked)
		} else {
			missing := 0
			for _, m := range v.Missing {
				missing += len(m)
			}
			return fmt.Errorf("verification failed: %d placements and %d lists are missing; re-run apply to finish",
				missing, len(v.MissingLists))
		}
	}
	return nil
}

// checkTokenOwner refuses to apply a plan through someone else's token. The
// plan names the account whose stars were read, but the writes go wherever the
// token points — and a mismatch would create lists on one account from a
// categorization of another's stars.
func checkTokenOwner(ctx context.Context, c *gh.ListsClient, planUser string) error {
	login, err := c.Login(ctx)
	if err != nil {
		return fmt.Errorf("checking which account the token belongs to: %w", err)
	}
	switch {
	case login == "":
		return fmt.Errorf("could not confirm which account the token belongs to; expected %s. "+
			"Refusing to write rather than risk creating lists on the wrong account", planUser)
	case !strings.EqualFold(login, planUser):
		return fmt.Errorf("this plan was built from %s's stars, but the token belongs to %s; "+
			"re-run `constellation plan` as %s, or use %s's token", planUser, login, login, planUser)
	}
	return nil
}

// stdinIsTerminal reports whether standard input is an interactive terminal
// rather than a pipe, a file, or /dev/null.
func stdinIsTerminal() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func confirm(p *plan.Plan, only []string, limit int, reconcile, force bool) (bool, error) {
	n, lists := p.Placements(), len(p.Categories)
	if len(only) > 0 {
		n, lists = 0, 0
		for _, c := range p.Categories {
			for _, o := range only {
				if strings.EqualFold(strings.TrimSpace(o), c.Name) {
					n += len(c.Repos)
					lists++
				}
			}
		}
	}
	if limit > 0 && limit < n {
		n = limit
	}
	fmt.Printf("\nThis will create up to %d star lists on github.com/%s and file %d repositories into them.\n",
		lists, p.User, n)
	// Consent has to cover the destructive half too: reconciling deletes the
	// categories the plan dropped, and --force deletes to make room.
	scoped := len(only) > 0
	switch {
	case reconcile && scoped:
		fmt.Println(ui.Warn("It will also remove repositories from the categories named above " +
			"when the plan no longer puts them there. No lists will be deleted."))
	case reconcile && force:
		fmt.Println(ui.Warn("It will also DELETE lists this tool created that the plan no longer has, " +
			"remove repositories from categories they have left, and delete further lists if room is needed."))
	case reconcile:
		fmt.Println(ui.Warn("It will also DELETE lists this tool created that the plan no longer has, " +
			"and remove repositories from categories they have left."))
	case force:
		fmt.Println(ui.Warn("If there is not enough room, it will DELETE lists this tool created that " +
			"the plan no longer has."))
	}
	fmt.Print("Continue? [y/N] ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return false, nil
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}

func runReset(ctx context.Context, token string, yes, dryRun, keepGo bool, delay time.Duration) error {
	c, err := gh.NewListsClient(token)
	if err != nil {
		return err
	}
	c.Log = progress
	user, err := c.Login(ctx)
	if err != nil {
		return err
	}

	if !yes && !dryRun {
		if !stdinIsTerminal() {
			return fmt.Errorf("reset deletes lists but stdin is not a terminal; re-run with --yes to proceed, or --dry-run to preview")
		}
		fmt.Printf("This will delete every star list constellation created on github.com/%s. Hand-made lists are left alone.\n", user)
		fmt.Print("Continue? [y/N] ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			fmt.Println(ui.Warn("aborted; nothing was changed"))
			return nil
		}
	}

	res, err := plan.Reset(ctx, c, user, plan.ResetOptions{
		DryRun: dryRun, ContinueOnError: keepGo, Delay: delay, Out: os.Stdout,
	})
	if res != nil {
		fmt.Printf("\n%d %s %s deleted, %d left alone\n",
			len(res.Deleted), plural(len(res.Deleted), "list", "lists"),
			pastTense(len(res.Deleted), dryRun), len(res.Kept))
		for _, e := range res.Errors {
			fmt.Fprintln(os.Stderr, " ", ui.Removed("!"), e)
		}
	}
	return err
}

func runShow(ctx context.Context, token string) error {
	c, err := gh.NewListsClient(token)
	if err != nil {
		return err
	}
	c.Log = progress
	user, err := c.Login(ctx)
	if err != nil {
		return err
	}
	// One query returns every list's metadata, so size, visibility and
	// description come for free rather than costing a request each.
	lists, err := c.Lists(ctx)
	if err != nil {
		return err
	}
	if len(lists) == 0 {
		fmt.Printf("%s has no star lists yet\n", user)
		return nil
	}
	for _, l := range lists {
		fmt.Printf("%s%s %8d  %-7s  %s\n", ui.Title(l.Name), ui.Pad(l.Name, 34),
			l.Count, visibilityColour(l.Private),
			ui.Muted(fmt.Sprintf("https://github.com/stars/%s/lists/%s", user, l.Slug)))
		// The description goes on its own line: it runs to 160 characters,
		// which no column width accommodates without truncating it.
		if d := strings.TrimSpace(l.Description); d != "" {
			fmt.Printf("  %s\n", ui.Muted(d))
		}
	}
	return nil
}

func progress(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

func visibilityColour(private bool) string {
	if private {
		return ui.Warn("private")
	}
	return ui.Muted("public")
}

// pastTense agrees with the count, so a summary does not read "1 list were
// deleted".
func pastTense(n int, dryRun bool) string {
	if dryRun {
		return "would be"
	}
	if n == 1 {
		return "was"
	}
	return "were"
}

// plural picks the right noun for a count, so a summary does not read
// "1 lists would be created".
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
