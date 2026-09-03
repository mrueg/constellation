package gh

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v5"
)

// Star lists live in GitHub's GraphQL schema: viewer.lists to read, and
// createUserList / updateUserList / deleteUserList / updateUserListsForItem to
// write. Reading needs no special scope; writing needs "user".
//
// This replaces an earlier implementation that replayed the web UI's HTML
// forms with a browser session cookie, because for a long time there was no
// API for lists at all. That approach had to guess list slugs, scrape
// membership out of markup with a regular expression, and treat an
// undocumented "406 with an empty body" as success — and it asked the user for
// a credential that is a full account login. None of that is needed now.

const graphqlEndpoint = "https://api.github.com/graphql"

// userAgent identifies the tool honestly. The previous session client sent a
// browser-shaped string, which reads as evasion; a named client with a link is
// both truthful and easier for GitHub to contact rather than block.
const userAgent = "constellation (+https://github.com/mrueg/constellation)"

// ErrNeedsUserScope means the token can read lists but not change them.
var ErrNeedsUserScope = fmt.Errorf(
	"changing star lists needs a token with the 'user' scope; run `gh auth refresh -s user`, " +
		"or set GITHUB_TOKEN to a token that has it")

// ListsClient talks to the GraphQL API.
type ListsClient struct {
	token string
	http  *http.Client
	// endpoint is overridable so tests can point at a local server.
	endpoint string
	Log      func(format string, args ...any)
}

// ListsOption adjusts a ListsClient. The endpoint override exists for GitHub
// Enterprise and for pointing tests at a local server, so there is no
// test-only constructor.
type ListsOption func(*ListsClient)

// WithHTTPClient supplies the HTTP client to use.
func WithHTTPClient(hc *http.Client) ListsOption {
	return func(c *ListsClient) { c.http = hc }
}

// WithEndpoint sends GraphQL somewhere other than api.github.com.
func WithEndpoint(url string) ListsOption {
	return func(c *ListsClient) { c.endpoint = url }
}

// NewListsClient resolves a token the same way the REST client does.
func NewListsClient(token string, opts ...ListsOption) (*ListsClient, error) {
	if token == "" {
		token = firstEnv("GITHUB_TOKEN", "GH_TOKEN")
	}
	if token == "" {
		token = ghCLIToken()
	}
	if token == "" {
		return nil, fmt.Errorf("no GitHub token: set GITHUB_TOKEN, or run `gh auth login`")
	}
	c := &ListsClient{
		token:    token,
		http:     &http.Client{Timeout: 60 * time.Second},
		endpoint: graphqlEndpoint,
	}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

func (c *ListsClient) logf(format string, args ...any) {
	if c.Log != nil {
		c.Log(format, args...)
	}
}

type graphQLError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Path    []any  `json:"path"`
}

// query runs one GraphQL operation and unmarshals data into out, failing if
// GitHub reported any error at all.
func (c *ListsClient) query(ctx context.Context, query string, vars map[string]any, out any) error {
	errs, err := c.queryPartial(ctx, query, vars, out)
	if err != nil {
		return err
	}
	if len(errs) > 0 {
		return graphQLFailure(errs)
	}
	return nil
}

func graphQLFailure(errs []graphQLError) error {
	msgs := make([]string, len(errs))
	tooLarge := false
	for i, e := range errs {
		msgs[i] = e.Message
		if strings.Contains(e.Message, "Resource limits") {
			tooLarge = true
		}
	}
	if tooLarge {
		return fmt.Errorf("GitHub returned: %s: %w", strings.Join(msgs, "; "), errQueryTooLarge)
	}
	return fmt.Errorf("GitHub returned: %s", strings.Join(msgs, "; "))
}

// queryPartial runs one operation, unmarshals whatever data came back, and
// hands the caller any errors alongside it rather than instead of it.
//
// GraphQL answers almost everything with HTTP 200, so the status code says
// little — but it also answers *partially*: asking for a hundred repositories
// where one has been deleted returns ninety-nine results and a NOT_FOUND for
// the last. Treating that as a plain failure threw away the ninety-nine, so a
// single deleted star aborted an entire apply.
func (c *ListsClient) queryPartial(ctx context.Context, query string, vars map[string]any, out any) ([]graphQLError, error) {
	return c.queryPartialN(ctx, query, vars, out, 0)
}

// maxRateLimitWaits bounds how long a single operation will sit waiting out
// the GraphQL budget, so a persistently exhausted account fails with a message
// rather than hanging.
const maxRateLimitWaits = 5

func (c *ListsClient) queryPartialN(ctx context.Context, query string, vars map[string]any, out any, waits int) ([]graphQLError, error) {
	body, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return nil, err
	}

	// Creating a list is the one operation that is not safe to replay: a
	// timed-out request GitHub actually processed would make a second list.
	// The others replace state and are idempotent.
	replayable := !strings.Contains(query, "createUserList")

	resp, err := waitOut(ctx, c.logf, func() (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, backoff.Permanent(err)
		}
		req.Header.Set("Authorization", "bearer "+c.token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", userAgent)
		resp, err := c.http.Do(req)
		if err != nil {
			if !replayable {
				return nil, backoff.Permanent(err)
			}
			return nil, err
		}
		if wait, ok := throttle(resp); ok {
			_ = resp.Body.Close()
			return nil, wait
		}
		// A 502 is what GitHub answers when a query takes too long, and it is
		// transient; without this it ended the run.
		if resp.StatusCode >= 500 {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("github returned %s", resp.Status)
		}
		return resp, nil
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("GitHub rejected the token: %s", strings.TrimSpace(string(raw)))
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("graphql request failed: %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}

	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []graphQLError  `json:"errors"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("parsing the GraphQL reply: %w", err)
	}
	for _, e := range envelope.Errors {
		// Exceeding the GraphQL budget arrives as HTTP 200 with an error in
		// the body, so the status-code check inside the retry never sees it.
		// Waiting is the only remedy, exactly as for the REST limits.
		if e.Type == "RATE_LIMITED" {
			if waits >= maxRateLimitWaits {
				return nil, fmt.Errorf("GitHub's GraphQL rate limit is still exhausted after %d waits: %s",
					waits, e.Message)
			}
			c.logf("GraphQL rate limit reached, waiting a minute")
			select {
			case <-time.After(time.Minute):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return c.queryPartialN(ctx, query, vars, out, waits+1)
		}
		// A missing scope is terminal however it is phrased: fine-grained
		// tokens report it as FORBIDDEN rather than INSUFFICIENT_SCOPES, and
		// without recognising it the run logs the same refusal once per
		// repository instead of stopping.
		if e.Type == "INSUFFICIENT_SCOPES" ||
			(e.Type == "FORBIDDEN" && strings.Contains(strings.ToLower(e.Message), "scope")) ||
			(e.Type == "FORBIDDEN" && strings.Contains(strings.ToLower(e.Message), "not accessible")) {
			return nil, ErrNeedsUserScope
		}
	}
	if out != nil && len(envelope.Data) > 0 {
		if err := json.Unmarshal(envelope.Data, out); err != nil {
			return nil, fmt.Errorf("parsing the GraphQL reply: %w", err)
		}
	}
	return envelope.Errors, nil
}
