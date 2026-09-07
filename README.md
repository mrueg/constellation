<h1 align="center">
  <img src="logo.png" alt="constellation — Your Stars. In Order." width="520">
</h1>

<p align="center">
  Groups your GitHub stars into categories with an unsupervised model, then
  creates the matching GitHub star lists and files each star into one.
</p>

<p align="center">
  <img src="demo/constellation.gif" alt="constellation grouping 4,169 stars into 31 lists, then filing them into GitHub star lists" width="900">
</p>

```sh
constellation plan     # group your stars and write a plan; changes nothing
constellation apply    # create the lists and file the stars into them
constellation show     # show the lists on your account
```

`plan` prints every category it found; `--show N` keeps the terminal readable
without changing what the plan file holds. The recording above is a real run,
with `apply` in `--dry-run` mode.

## What it does

1. Reads every repository you have starred through the GitHub REST API,
   including topics, language and description.
2. Embeds each one as a vector and clusters them — the model is unsupervised,
   so the categories come out of *your* stars rather than a fixed taxonomy.
3. Names each cluster from the terms that distinguish it from the others.
4. Writes a plan you can read, edit and keep.
5. On `apply`, creates the lists on GitHub and files the repositories.

Planning never touches your account.

## Install

```sh
go install github.com/mrueg/constellation@latest
```

Or take a binary from the [releases](https://github.com/mrueg/constellation/releases)
page, built for linux and macOS on amd64 and arm64.

## Credentials

`constellation` uses `$GITHUB_TOKEN`, then `$GH_TOKEN`, then whatever the `gh`
CLI has stored, so if you already run `gh auth login` there is nothing to set
up. `--token` overrides all three.

**Reading** needs no scopes for public stars; add `repo` if you have starred
private repositories you want included.

**Changing lists** needs the `user` scope:

```sh
gh auth refresh -s user
```

Without it, reads still work and `apply` stops with a message naming the
command above rather than failing halfway through.

## The model

Repositories are turned into a term matrix and factored with a truncated SVD —
latent semantic analysis. Term matching alone only groups repositories that
share vocabulary, so a "container runtime" and an "OCI sandbox" stay apart
however obviously they belong together; the factorization replaces terms with a
few hundred latent directions built from terms that co-occur, and the two land
together without sharing a word. It takes about a minute on a few thousand
stars and needs no setup.

## READMEs

A repository description is nine words at the median on a real account, and a
third of repositories carry no topics at all, so by default `constellation`
also reads each README — one API call per repository, cached afterwards, so the
cost is paid once. `--readme=false` skips it.

The READMEs are cached in a file of their own, keyed by repository, and live
for 30 days. That is deliberate: the star list goes stale in a day and is a few
dozen cheap pages, while a README costs an API call each and thousands of them
are what a first run actually spends its time on. Under one shared expiry,
picking up a handful of new stars threw every README away with the star list
and re-read the lot. `--refresh` now re-fetches only the stars;
`--refresh-readmes` re-reads the prose.

Only the opening `--readme-words` (120) are kept: a project's first paragraph
is its own summary of itself, while the rest is installation and contribution
boilerplate that every repository shares and that would group them by nothing.

`--readme-weight` matters more than it looks. A README contributes a hundred or
so terms against a description's nine, so weighting them evenly lets prose
drown the curated topics that carry the most signal per term. Measured against
repository topics on a 4,169-star account:

| README weight per term | agreement |
| --- | --- |
| 0 (no README) | 0.0490 |
| **0.15 (default)** | **0.0498** |
| 0.5 | 0.0439 |

The gain at 0.15 is small — close to noise — but the loss at 0.5 is not, which
is the reason the default is low rather than absent.

## Repositories in more than one list

GitHub star lists are many-to-many, and a Rust command line tool genuinely
belongs under both headings — but every algorithm above returns a partition.
`--multi-list N` lets a repository join up to N lists, and
`--multi-list-ratio F` sets how close a further category must fit, as a
fraction of the best fit, to earn it.

It is decided on how a repository's similarity to another centroid compares
with the similarity to its own, rather than on an absolute cosine. A ratio
means the same thing whatever the backend and however many dimensions it
produces, whereas a fixed threshold is differently strict for a tight category
than for a broad one.

Note the trade: measured against repository topics on a 4,169-star account,
turning this on put 738 repositories into a second list and lowered
within-list topic agreement from 0.049 to 0.044. Second-choice placements are
weaker fits by definition — this buys coverage, not purity.

## Tuning the categories

> **The measured figures in this section predate two model fixes** — term
> weights below one used to come out negative, and the dendrogram was cut in
> the order merges were produced rather than by height. Both changed the
> output, as did a later fix to keep centroids describing their members: the
> same defaults now place 3,369 of 4,169 stars where they placed 2,993. Treat
> the numbers below as directional until they are re-measured.


The defaults aim at lists you would plausibly have made by hand.

| Flag | Default | What it does |
| --- | --- | --- |
| `--algorithm` | `agglomerative` | Ward agglomerative clustering, or `kmeans`. Several flags below apply only to one of them. |
| `--clusters N` | `0` | Fixes the number of categories. `0` cuts the dendrogram at `--max-clusters`; under `kmeans` it searches by silhouette score. |
| `--min-clusters` / `--max-clusters` | `6` / `32` | `--max-clusters` is where the tree is cut. `--min-clusters` applies to `kmeans` only. |
| `--min-size N` | `4` | Dissolves categories smaller than this. A three-repo list is not a category. |
| `--max-lists N` | `32` | Keeps only the N largest categories. See the cap below. |
| `--lsa-dims N` | `150` | Latent dimensions kept by the factorization. |
| `--consensus N` | `1` | Cluster the agreement between N runs. See below. |
| `--min-cohesion F` | `0.36` | Dissolve a category whose members do not resemble it this much. |
| `--split-parts N` | `3` | Pieces an incoherent category is divided into before being dissolved. |
| `--language-weight F` | `0` | How much the detected language counts. See below. |
| `--multi-list N` | `1` | Most lists one repository may join. |
| `--outlier-sigmas F` | `3` | How readily a repository is left uncategorized. `0` forces everything into a list. |
| `--min-similarity F` | `0.05` | Absolute floor on fit, under the outlier test. |
| `--seed N` | `1` | Random seed for `--algorithm kmeans`. The default clustering ignores it. |
| `--lsa-seed N` | `1` | Seeds the randomized factorization. This one the default path does use — see above. |
| `--lsa-oversample N` | `0` | Extra columns in the random sketch; `0` scales with `--lsa-dims`. Wider is steadier and slower. |
| `--lsa-power N` | `4` | Power iterations in the SVD. More is slower and sharper. |
| `--topic-weight F` | `3` | How much a repository's own topics count. The strongest single signal. |
| `--topic-word-weight F` | `1.2` | How much the individual words of a topic count, on top of the whole topic. |
| `--topic-inferred-weight F` | `0.5` | How much topics *guessed* from a name or description count, against real ones. |
| `--topic-min-count N` | `3` | How often a topic must appear before it can be inferred elsewhere. |
| `--readme-weight F` | `0.15` | How much a README word counts. Raising it measured worse; see above. |
| `--readme-words N` | `120` | Words kept from each README. |
| `--readme-bytes N` | `8192` | Bytes read from each README before truncation. |
| `--readme-workers N` | `8` | Concurrent README fetches. |
| `--min-df N` / `--max-df F` | `2` / `0.4` | Ignore terms rarer or more common than this. |
| `--max-vocab N` | `12000` | Cap on vocabulary size. `0` lifts it — needed with `--min-df 1`. |
| `--weighting` | `tfidf` | `tfidf` or `bm25`, with `--bm25-k1` and `--bm25-b`. |
| `--merge-duplicates F` | `0.3` | Merge two categories sharing this fraction of their top terms. |
| `--min-name-coverage F` | `0.15` | Drop a category whose name describes too few of its members. |
| `--split-max-size N` / `--split-min-size N` | `200` / `40` | When an oversized category is split, and how small the pieces may be. |
| `--rescue-neighbours N` | `10` | Neighbours polled when placing a repository no category claimed. |
| `--rescue-agreement F` | `0.8` | How many of them must agree before it is placed. |
| `--verbose`, `-v` | | Prints every repository rather than five per category. |
| `--show N` | `0` | Print only the first N categories. The plan file always holds every one. |

`--out` and `--plan` set where the plan is written and read, `--only` restricts
an apply to named categories, and `plan --apply` runs both halves in one go.
`--restarts` and `--silhouette-sample` tune `--algorithm kmeans` only.

**The detected language is not counted as evidence.** Every repository written
in Go shares one identical term, which makes it far too strong a grouper: it
was fusing Go with Wardley mapping into one 353-repository list, and C with
Zsh. Dropping it raised topic agreement from 0.0537 to 0.0588, cut categories
named after two unrelated things from ten to five, and split that list into a
clean **Go** and a separate **Wardley Mapping**. A language the author *tagged*
as a topic still counts at topic weight — that is a deliberate label rather
than a detection. `--language-weight 1.5` restores the old behaviour if you
would rather have language-shaped lists.

**A fixed seed always reproduces its own answer — but the seed matters more
than any other flag.** The clustering itself has no random element, so the same
stars and the same flags give byte-identical output. What is seeded is the
randomized SVD underneath it, and its `--lsa-seed` moves the result further
than any documented knob. Before the sketch was widened, three seeds placed
3663, 3398 and 3112 of the same 4169 stars; they now place 3284, 3543 and 3477.

That is partly under-convergence, since a randomized factorization is only an
approximation and the sketch was too narrow for 150 dimensions. Widening it
(now `--lsa-dims / 2`, adjustable with `--lsa-oversample`) roughly halved the
spread. The rest is a property of the stars: several roughly equally good ways
to divide them into thirty groups exist, and the seed picks between them. Read
the result as one reasonable cut, not as the cut.

`--consensus N` clusters the agreement between N runs rather than trusting one,
at N times the cost, and applies only under `--algorithm kmeans`.

**The defaults were chosen by measurement, not taste**, and a single run is not
a measurement — the metric moves by 0.006 between seeds with nothing else
changed, which is larger than most differences worth arguing about. The figures
below are medians across seeds. Scored against
repository topics on a 4,169-star account — how much more topic overlap two
repositories in the same list have than two picked at random:

| Setting | Lists | Sizes | Agreement |
| --- | --- | --- | --- |
| `--lsa-dims 200` (previous default) | 30 | 56–381 | 0.0496 |
| `--lsa-dims 150` (default) | 32 | 60–358 | 0.0542 |
| `--lsa-dims 100` | 32 | 31–318 | 0.0554 |
| `--min-df 5` | 28 | 57–525 | 0.0554 |
| `--readme-words 40` | 32 | 71–215 | 0.0515 |

`--lsa-dims 100` and `--min-df 5` score highest, but the first leaves more
repositories uncategorized and the second produces a 525-repository list, so
the default sits at 150 — nearly all of the gain with neither cost.

These knobs **interact badly**: every pairing tried scored below either setting
on its own (`--min-df 5 --lsa-dims 100` gives 0.0524, `--min-df 5
--readme-words 40` gives 0.0457). Change one at a time.

**GitHub allows 32 star lists per account.** That is the reason `--max-clusters`
and `--max-lists` default to 32 rather than to something open-ended. Lists you
already have count against the same limit, so if you have ten, only 22 new ones
fit — `apply` checks this before it writes anything and tells you what to
re-run, rather than discovering it two thirds of the way through.

**On the repositories left uncategorized.** Every star list has a long tail
that belongs to no category — the one-off you starred from a link two years
ago. `constellation` reports those instead of forcing them somewhere, because a
category with three wrong members in it stops being useful. `--outlier-sigmas 0`
turns that off if you would rather have everything filed.

**How categories get their names.** The label comes from the terms that
distinguish a cluster from the others, with three refinements that matter more
than they sound:

Capitalization is learned from the corpus rather than applied by rule. No rule
produces "gRPC", "PostgreSQL" or "eBPF", and title-casing produces "Mcp" and
"Cidr". Descriptions and repository names carry the right spelling — GitHub
lower-cases every topic, so topics cannot supply it — and the corpus majority is
taken only when the spelling is *distinctive*, meaning a capital somewhere other
than the front. Otherwise ordinary prose, which writes words in lower case
mid-sentence, would name a list "container".

A phrase built around the leading term beats joining two terms with an
ampersand, so a cluster whose terms are "Actions, GitHub, GitHub Actions" is
called **GitHub Actions** rather than "Actions & GitHub".

Words describing how a project is run rather than what it is about —
`hacktoberfest`, `good-first-issue`, `oss` — are excluded from names, though
they stay in the vectors. A list called "Hacktoberfest" tells its reader
nothing.

An ampersand in a name is often a fair warning: it usually means the cluster
really does contain two things, and the low cohesion beside it says the same.

The plan is plain JSON. Renaming a category, deleting one, or moving a
repository between them before you apply is expected — the names a model
derives from term statistics are a starting point, not a verdict.

`--markdown stars.md` also writes the categories as a linked index, which is
useful on its own even if you never apply the plan.

## Applying safely

```sh
constellation apply --dry-run      # what would change, touching nothing
constellation apply --limit 5      # write five repositories, check the result
constellation apply --only 'Kubernetes' --only 'Rust'
constellation apply               # the rest
```

`apply` asks for confirmation, paces its writes (`--delay`, default 1s, which
is GitHub's documented minimum between mutating requests), skips repositories already in the right list, and is
safe to re-run — an interrupted run continues where it stopped.

GitHub allows 32 star lists per account. If the plan needs more room than is
left, `apply` refuses and says so. `--reconcile` frees room by itself, since it
deletes the categories the plan has dropped; `--force` does the same thing
without reconciling — it deletes lists this tool created that the plan no
longer contains, smallest first, and only as many as are needed to fit. Neither
ever touches a list you made yourself.

`--batch N` (default 25) puts several membership changes in one request.
GraphQL runs them serially, so the outcome is identical to sending them one at
a time — but GitHub's limits are on requests, which is what decides how long a
large apply takes. A request GitHub judges too complex is split and retried, so
the number is a target rather than a limit.

`--only` narrows a run to the categories you name, and `--reconcile` narrows
with it: the named lists are brought exactly in line, and nothing else is
touched — in particular no list is deleted, since a category the plan has
dropped is not one you can name. Without `--only`, reconciling is account-wide
as before. `--force` has no narrower meaning, because the lists it would delete
to make room are precisely the ones `--only` cannot name, so the two are
refused together.

`--dry-run` reports what would happen and writes nothing. It is the only way to
preview `--reconcile`, which deletes lists — `--limit` cannot stand in for it,
because reconciling deletes before any repository is written, so the two are
refused together rather than offering a safety belt that does not hold.

Lists are created public, as they are on GitHub. `--private` creates them
private instead. Visibility is enforced on every run rather than only at
creation, so the flag works in both directions: setting it makes existing lists
private, clearing it makes them public again. Only lists this tool manages are
touched either way.

To decide what is already filed, `apply` reads each list once rather than
opening every repository's membership dialog. A list page covers thirty
repositories, so a re-run over an unchanged account costs a few hundred
requests instead of one per starred repository.

It reads each repository's current list membership before writing it back.
GitHub's dialog submits the *complete* set of lists a repository belongs to, so
this is what stops an apply from silently emptying lists you curated by hand.

The CLI is built with [urfave/cli](https://github.com/urfave/cli), so
`constellation <command> --help` lists every flag, and both `-flag` and
`--flag` are accepted.

If the token cannot write — no `user` scope — `apply` stops with exit code 3
and names the command that fixes it, rather than repeating the same refusal for
every repository. Grant the scope and run the same command again: applying is
idempotent and resumes where it left off.

## Undoing it

```sh
constellation reset --dry-run   # what would be deleted
constellation reset             # delete them
```

`reset` deletes only the lists constellation created, recognised by the marker
every description it writes ends with. Lists you made by hand are never
touched, and the repositories stay starred — only the grouping goes.

## Other flags worth knowing

| Flag | Command | What it does |
| --- | --- | --- |
| `--adopt` | `apply` | Take over an existing list whose name matches a category but which this tool did not create. Without it such a list is skipped and reported, so a hand-curated list is never silently overwritten. |
| `--verify` | `apply` | Read the lists back afterwards and report any placement that did not land. |
| `--yes` / `-y` | `apply`, `reset` | Skip the confirmation prompt. Required for any non-interactive run. |
| `--continue` | `apply` | Keep going when one repository fails instead of stopping. |
| `--include-forks`, `--include-archived` | `plan` | Include stars that are otherwise filtered out; the run reports how many it skipped. |
| `--cache`, `--cache-ttl`, `--refresh` | `plan` | The star cache lives under your user cache directory (`stars.json`), is reused for 24 hours, and `--refresh` ignores it. |
| `--readme-cache`, `--readme-cache-ttl`, `--refresh-readmes` | `plan` | The README cache is a separate file (`readmes.json`) with its own 30-day life, so re-fetching your stars does not re-read every README. `0` keeps them forever. |

## License

Apache License 2.0. See [LICENSE](LICENSE).
