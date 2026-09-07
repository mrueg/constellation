// Package gh talks to GitHub: the documented REST API for everything it
// covers; star lists go through the GraphQL API in listops.go.
package gh

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v5"
	"github.com/google/go-github/v90/github"
)

// Repo is the subset of a starred repository that the categorizer reads. It is
// deliberately its own type rather than go-github's: it is what gets cached on
// disk, and the cache should not have to change when the API client does.
type Repo struct {
	ID          int64     `json:"id"`
	FullName    string    `json:"full_name"`
	Description string    `json:"description"`
	Language    string    `json:"language"`
	Topics      []string  `json:"topics"`
	Stars       int       `json:"stargazers_count"`
	HTMLURL     string    `json:"html_url"`
	Archived    bool      `json:"archived"`
	Fork        bool      `json:"fork"`
	PushedAt    time.Time `json:"pushed_at"`
	StarredAt   time.Time `json:"starred_at"`
	Readme      string    `json:"readme,omitempty"`
}

// Owner returns the account or organization that owns the repository.
func (r Repo) Owner() string {
	if i := strings.IndexByte(r.FullName, '/'); i > 0 {
		return r.FullName[:i]
	}
	return ""
}

// Name returns the repository name without its owner.
func (r Repo) Name() string {
	if i := strings.IndexByte(r.FullName, '/'); i >= 0 {
		return r.FullName[i+1:]
	}
	return r.FullName
}

func newRepo(r *github.Repository, starredAt time.Time) Repo {
	return Repo{
		ID:          r.GetID(),
		FullName:    r.GetFullName(),
		Description: r.GetDescription(),
		Language:    r.GetLanguage(),
		Topics:      r.Topics,
		Stars:       r.GetStargazersCount(),
		HTMLURL:     r.GetHTMLURL(),
		Archived:    r.GetArchived(),
		Fork:        r.GetFork(),
		PushedAt:    r.GetPushedAt().Time,
		StarredAt:   starredAt,
	}
}

// Client reads from the GitHub REST API.
type Client struct {
	api *github.Client
	// Log receives progress lines; nil discards them.
	Log func(format string, args ...any)
}

// NewClient builds a client from an explicit token, the GITHUB_TOKEN /
// GH_TOKEN environment variables, or the gh CLI's stored credentials, in that
// order.
func NewClient(token string) (*Client, error) {
	if token == "" {
		token = firstEnv("GITHUB_TOKEN", "GH_TOKEN")
	}
	if token == "" {
		token = ghCLIToken()
	}
	if token == "" {
		return nil, fmt.Errorf("no GitHub token: set GITHUB_TOKEN, or run `gh auth login`")
	}
	api, err := github.NewClient(
		github.WithAuthToken(token),
		github.WithUserAgent("constellation"),
		github.WithTimeout(60*time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("building the GitHub client: %w", err)
	}
	return &Client{api: api}, nil
}

func (c *Client) logf(format string, args ...any) {
	if c.Log != nil {
		c.Log(format, args...)
	}
}

// ghCLIToken asks the gh CLI for its stored token.
//
// It runs under a deadline because this happens before the signal handler is
// in play: a gh that hangs — a stuck credential helper, an unreachable
// keyring — would otherwise hang the whole program with no way to interrupt
// it. Failing here is not fatal; the caller falls through to reporting that no
// token was found.
func ghCLIToken() string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gh", "auth", "token").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Viewer returns the login of the authenticated user.
func (c *Client) Viewer(ctx context.Context) (string, error) {
	var login string
	err := c.retry(ctx, func() error {
		u, _, err := c.api.Users.Get(ctx, "")
		if err != nil {
			return err
		}
		login = u.GetLogin()
		return nil
	})
	if err != nil {
		return "", err
	}
	return login, nil
}

// starsPerPage is the page size every star walk uses. The incremental fetch
// depends on it, since it works out which page a cached star list ends on.
const starsPerPage = 100

// Starred fetches every repository the authenticated user has starred.
func (c *Client) Starred(ctx context.Context) ([]Repo, error) {
	return c.starredFrom(ctx, 1)
}

// starredFrom walks the starred list from the given page onwards.
func (c *Client) starredFrom(ctx context.Context, from int) ([]Repo, error) {
	// Oldest first. GitHub's default for this endpoint is newest first, and
	// under that order a star added while the pages are being walked pushes
	// every later item down one slot — so one repository is silently skipped,
	// and unstarring duplicates one instead. Ascending, new stars land past
	// the cursor and disturb nothing already read — and a cached prefix stays
	// where it was, which is what makes an incremental fetch possible at all.
	opts := &github.ActivityListStarredOptions{
		Sort:        "created",
		Direction:   "asc",
		ListOptions: github.ListOptions{PerPage: starsPerPage, Page: from},
	}
	var all []Repo
	// Belt and braces: a repository seen twice would be clustered twice and
	// filed twice.
	seen := make(map[int64]bool)
	for page := from; ; page++ {
		var (
			items []*github.StarredRepository
			resp  *github.Response
		)
		err := c.retry(ctx, func() error {
			var err error
			items, resp, err = c.api.Activity.ListStarred(ctx, "", opts)
			return err
		})
		if err != nil {
			return nil, fmt.Errorf("listing starred repositories: %w", err)
		}
		for _, it := range items {
			if it.Repository == nil || seen[it.Repository.GetID()] {
				continue
			}
			seen[it.Repository.GetID()] = true
			all = append(all, newRepo(it.Repository, it.GetStarredAt().Time))
		}
		c.logf("fetched %d stars (page %d)", len(all), page)
		if resp == nil || resp.NextPage == 0 {
			return all, nil
		}
		opts.Page = resp.NextPage
	}
}

// StarredIncremental brings a cached star list up to date by re-reading only
// its last page onwards, and reports whether that was sound.
//
// Stars come back oldest first, so everything already cached keeps its
// position and new ones land at the end: reading the final page again and
// walking on from there is enough to find them, at two requests instead of one
// per hundred stars.
//
// What that assumes is that nothing was removed. Unstarring a repository
// shifts every later one down a slot, and an unstar early in the list would
// otherwise go unnoticed for as long as the cache lived — leaving the plan to
// file a repository the account no longer stars. So the overlap is checked
// rather than trusted: the page that should still hold the cached tail has to
// hold exactly it, in order. It does not, ok is false and the caller re-reads
// the list in full, which is the same work the old code did unconditionally.
func (c *Client) StarredIncremental(ctx context.Context, cached []Repo) ([]Repo, bool, error) {
	// Below a page there is nothing to save: the full walk is one request.
	if len(cached) < starsPerPage {
		return nil, false, nil
	}
	// The page the cached list ends on, so that the last cached page is read
	// again — that overlap is what the check below is made of — and everything
	// after it is new.
	from := (len(cached) + starsPerPage - 1) / starsPerPage
	offset := (from - 1) * starsPerPage

	tail, err := c.starredFrom(ctx, from)
	if err != nil {
		return nil, false, err
	}
	overlap := len(cached) - offset
	if len(tail) < overlap {
		return nil, false, nil // stars disappeared; the prefix cannot be trusted either
	}
	for i, r := range cached[offset:] {
		if tail[i].ID != r.ID {
			return nil, false, nil
		}
	}
	// The cached prefix, then everything from the overlap on as the API now
	// reports it — so an unstar inside the tail is picked up as well.
	merged := make([]Repo, 0, offset+len(tail))
	merged = append(merged, cached[:offset]...)
	merged = append(merged, tail...)
	return merged, true, nil
}

// Readme fetches the repository's README as plain text, truncated to limit
// bytes. A missing README is not an error; some repositories have none.
func (c *Client) Readme(ctx context.Context, owner, repo string, limit int) (string, error) {
	var content string
	err := c.retry(ctx, func() error {
		rc, _, err := c.api.Repositories.GetReadme(ctx, owner, repo, nil)
		if err != nil {
			return err
		}
		content, err = rc.GetContent()
		return err
	})
	if err != nil {
		if isNotFound(err) {
			return "", nil
		}
		return "", err
	}
	if len(content) > limit {
		content = content[:limit]
	}
	return stripMarkdown(content), nil
}

// retry waits out rate limits rather than failing a long fetch halfway
// through. go-github reports both the primary limit and the secondary "abuse"
// limit as typed errors carrying the time to wait, so the delay asked for is
// the one GitHub named rather than a guess.
func (c *Client) retry(ctx context.Context, call func() error) error {
	_, err := waitOut(ctx, c.Log, func() (struct{}, error) {
		err := call()
		if err == nil {
			return struct{}{}, nil
		}
		return struct{}{}, classify(err)
	})
	return err
}

// classify turns a GitHub error into an instruction for the retry loop.
func classify(err error) error {
	var abuse *github.AbuseRateLimitError
	if errors.As(err, &abuse) {
		if d := abuse.GetRetryAfter(); d > 0 {
			return backoff.RetryAfter(int(d.Seconds()) + 1)
		}
		return err
	}
	var limit *github.RateLimitError
	if errors.As(err, &limit) {
		if d := time.Until(limit.Rate.Reset.Time); d > 0 && d < time.Hour {
			return backoff.RetryAfter(int(d.Seconds()) + 1)
		}
		return backoff.Permanent(err)
	}
	var resp *github.ErrorResponse
	if errors.As(err, &resp) && resp.Response != nil && resp.Response.StatusCode >= 500 {
		return err
	}
	return backoff.Permanent(err)
}

func isNotFound(err error) bool {
	var resp *github.ErrorResponse
	return errors.As(err, &resp) && resp.Response != nil && resp.Response.StatusCode == http.StatusNotFound
}

func firstEnv(names ...string) string {
	for _, n := range names {
		if v := strings.TrimSpace(os.Getenv(n)); v != "" {
			return v
		}
	}
	return ""
}
