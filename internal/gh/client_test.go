package gh

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cenkalti/backoff/v5"
	"github.com/google/go-github/v90/github"
)

// fastRetries shortens the retry curve for the test, so a retried request
// costs milliseconds rather than the seconds a real one waits.
func fastRetries(t *testing.T) {
	t.Helper()
	interval, budget := retryInitialInterval, retryMaxWait
	retryInitialInterval = time.Millisecond
	retryMaxWait = 2 * time.Second
	t.Cleanup(func() { retryInitialInterval, retryMaxWait = interval, budget })
}

// apiResponse is the HTTP half of a go-github error. Its Error method reads
// the request, so one is supplied.
func apiResponse(status int) *http.Response {
	return &http.Response{
		StatusCode: status,
		Request:    &http.Request{Method: http.MethodGet, URL: &url.URL{Scheme: "https", Host: "api.github.com", Path: "/x"}},
	}
}

func apiError(status int) *github.ErrorResponse {
	return &github.ErrorResponse{Response: apiResponse(status), Message: http.StatusText(status)}
}

func rateLimit(reset time.Time) *github.RateLimitError {
	return &github.RateLimitError{
		Rate:     github.Rate{Remaining: 0, Reset: github.Timestamp{Time: reset}},
		Response: apiResponse(http.StatusForbidden),
		Message:  "API rate limit exceeded",
	}
}

// classify decides what the retry loop does with a failure, and the wrong
// verdict is costly in both directions: retrying a bad token wastes the
// budget, while giving up on a timeout failed a request that would have
// succeeded a second later — and, upstream, got its README cached as
// unreadable for a month.
func TestClassify(t *testing.T) {
	fastRetries(t)
	transportTimeout := &url.Error{Op: "Get", URL: "https://api.github.com/x", Err: errors.New("dial tcp: i/o timeout")}
	for _, tc := range []struct {
		name      string
		err       error
		permanent bool
		is        error // a sentinel the result must wrap, if any
		after     bool  // the result must ask for a named delay
	}{
		{name: "transport error", err: transportTimeout},
		{name: "connection reset", err: fmt.Errorf("reading body: %w", io.ErrUnexpectedEOF)},
		{name: "server error", err: apiError(http.StatusBadGateway)},
		{name: "bad credentials", err: apiError(http.StatusUnauthorized), permanent: true, is: ErrUnauthorized},
		{name: "forbidden", err: apiError(http.StatusForbidden), permanent: true},
		{name: "unprocessable", err: apiError(http.StatusUnprocessableEntity), permanent: true},
		{name: "unknown", err: errors.New("something else"), permanent: true},
		{name: "rate limit within budget", err: rateLimit(time.Now().Add(time.Second)), after: true},
		{name: "rate limit beyond budget", err: rateLimit(time.Now().Add(time.Hour)), permanent: true, is: ErrRateLimited},
		{name: "abuse limit", err: &github.AbuseRateLimitError{Response: apiResponse(http.StatusForbidden), RetryAfter: github.Ptr(30 * time.Second)}, after: true},
	} {
		got := classify(tc.err)
		var permanent *backoff.PermanentError
		if errors.As(got, &permanent) != tc.permanent {
			t.Errorf("%s: permanent = %v, want %v (%v)", tc.name, !tc.permanent, tc.permanent, got)
		}
		var after *backoff.RetryAfterError
		if errors.As(got, &after) != tc.after {
			t.Errorf("%s: asks for a delay = %v, want %v (%v)", tc.name, !tc.after, tc.after, got)
		}
		if tc.is != nil && !errors.Is(got, tc.is) {
			t.Errorf("%s: %v does not wrap %v", tc.name, got, tc.is)
		}
		// Whatever the verdict, the original error must still be there:
		// it is what tells the user why.
		if !errors.Is(got, tc.err) {
			t.Errorf("%s: %v lost the original error", tc.name, got)
		}
	}

	// The reason must survive as a typed error too, so a caller can read the
	// reset time off it.
	var limit *github.RateLimitError
	if got := classify(rateLimit(time.Now().Add(time.Hour))); !errors.As(got, &limit) {
		t.Errorf("the rate-limit error was lost behind the sentinel: %v", got)
	}
}

// A rate limit whose reset is inside the budget when first seen can still
// outlast it once the loop has spent time on other retries; backoff then
// returns the bare "retry after" it was last handed. The caller must still be
// told it was the rate limit.
func TestRateLimitThatOutlastsTheBudgetIsNamed(t *testing.T) {
	fastRetries(t)
	// The reset is inside the budget, so classify asks to wait for it — but
	// the wait plus what has elapsed is more than the budget allows, and
	// backoff gives up on the spot.
	reset := time.Now().Add(retryMaxWait - 500*time.Millisecond)
	c := testRESTClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", fmt.Sprint(reset.Unix()))
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"API rate limit exceeded"}`)
	}))
	_, err := c.Readme(context.Background(), "a", "one", ReadmeLimits{Words: 120, Bytes: 100})
	if !errors.Is(err, ErrRateLimited) {
		t.Errorf("Readme under an exhausted quota returned %v, want ErrRateLimited", err)
	}
	if errors.Is(err, ErrReadmeUnavailable) {
		t.Errorf("a rate limit was reported as the README being unavailable: %v", err)
	}
}

// Readme separates the failures that are about the README from the ones that
// are about the moment. Only the former wrap ErrReadmeUnavailable; a caller
// that remembers them must not be handed a timeout or a bad token in the same
// clothes.
func TestReadmeReportsUnavailableFiles(t *testing.T) {
	fastRetries(t)
	for _, tc := range []struct {
		name        string
		status      int
		body        string
		unavailable bool
		ok          bool
	}{
		{name: "too large", status: http.StatusForbidden, body: `{"message":"This API returns blobs up to 1 MB in size. The requested blob is too large to fetch via the API."}`, unavailable: true},
		{name: "blocked", status: http.StatusForbidden, body: `{"message":"Repository access blocked","block":{"reason":"tos"}}`, unavailable: true},
		{name: "taken down", status: http.StatusUnavailableForLegalReasons, body: `{"message":"Repository access blocked","block":{"reason":"dmca"}}`, unavailable: true},
		{name: "unsupported encoding", status: http.StatusOK, body: `{"encoding":"none","content":null,"size":2000000}`, unavailable: true},
		{name: "no readme", status: http.StatusNotFound, body: `{"message":"Not Found"}`, ok: true},
		{name: "readable", status: http.StatusOK, body: `{"encoding":"base64","content":"aGVsbG8="}`, ok: true},
		{name: "bad credentials", status: http.StatusUnauthorized, body: `{"message":"Bad credentials"}`},
		{name: "forbidden for the caller", status: http.StatusForbidden, body: `{"message":"Resource protected by organization SAML enforcement."}`},
	} {
		c := testRESTClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			fmt.Fprint(w, tc.body)
		}))
		text, err := c.Readme(context.Background(), "a", "one", ReadmeLimits{Words: 120, Bytes: 100})
		if tc.ok {
			if err != nil {
				t.Errorf("%s: %v", tc.name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: no error, got %q", tc.name, text)
			continue
		}
		if errors.Is(err, ErrReadmeUnavailable) != tc.unavailable {
			t.Errorf("%s: unavailable = %v, want %v (%v)", tc.name, !tc.unavailable, tc.unavailable, err)
		}
	}
}

// A connection that drops is retried, not reported. The first answer is cut
// off mid-body; the second is fine.
func TestReadmeRetriesATransportFailure(t *testing.T) {
	fastRetries(t)
	hits := 0
	c := testRESTClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		if hits == 1 {
			// Announce a body and then hang up, which the client sees as
			// an unexpected EOF.
			w.Header().Set("Content-Length", "100")
			fmt.Fprint(w, `{"enc`)
			return
		}
		fmt.Fprint(w, `{"encoding":"base64","content":"aGVsbG8="}`)
	}))
	text, err := c.Readme(context.Background(), "a", "one", ReadmeLimits{Words: 120, Bytes: 100})
	if err != nil {
		t.Fatalf("a dropped connection was not retried: %v (after %d attempts)", err, hits)
	}
	if text != "hello" || hits != 2 {
		t.Errorf("got %q after %d attempts, want \"hello\" after 2", text, hits)
	}
}

// A secondary rate limit without a Retry-After used to go back on the
// exponential curve, which starts at two seconds — and hitting the limit again
// that soon is what escalates it into a longer ban. The documentation's minute
// applies, and the reason has to survive so the log can say what happened.
func TestClassifySecondaryLimitWithoutRetryAfterWaitsAMinute(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://api.github.com/user/starred", nil)
	if err != nil {
		t.Fatal(err)
	}
	abuse := &github.AbuseRateLimitError{
		Response: &http.Response{StatusCode: http.StatusForbidden, Request: req},
		Message:  "You have exceeded a secondary rate limit",
	}
	got := classify(abuse)

	var after *backoff.RetryAfterError
	if !errors.As(got, &after) {
		t.Fatalf("classify = %v, want a RetryAfter", got)
	}
	if after.Duration != time.Minute {
		t.Errorf("wait = %s, want 1m0s", after.Duration)
	}
	if !strings.Contains(got.Error(), "secondary rate limit") {
		t.Errorf("error lost GitHub's reason: %v", got)
	}

	// With a Retry-After, GitHub's own figure is still what is used.
	ten := 10 * time.Second
	abuse.RetryAfter = &ten
	if !errors.As(classify(abuse), &after) || after.Duration != 11*time.Second {
		t.Errorf("classify with Retry-After = %v, want an 11s wait", classify(abuse))
	}
}

// The markup is stripped and the opening chosen before the byte cap applies.
// This README's first 10 KB is badges, a centred HTML header and a table of
// contents, and the cap is 8 KB: cutting first, as the old code did, left
// nothing but shields.io URLs to tokenize.
func TestReadmeStripsBeforeItTruncates(t *testing.T) {
	fastRetries(t)
	markdown := badgeWall(10*1024) + "## Overview\n\n" + orbitSummary + "\n\n## Installation\n\n```sh\ngo install github.com/acme/orbit@latest\n```\n"
	c := testRESTClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"encoding":"base64","content":%q}`, base64.StdEncoding.EncodeToString([]byte(markdown)))
	}))
	text, err := c.Readme(context.Background(), "a", "one", ReadmeLimits{Words: 120, Bytes: 8192})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "Orbit is a lightweight job scheduler") {
		t.Errorf("the summary past the badges was lost: %q", text)
	}
	for _, noise := range []string{"https", "img", "shields", "go install"} {
		if strings.Contains(text, noise) {
			t.Errorf("%q reached the tokenizer: %q", noise, text)
		}
	}
	if len(text) > 8192 {
		t.Errorf("the byte cap was not applied: %d bytes", len(text))
	}
}

// starPage renders the starred-repositories response for the given ids.
func starPage(ids []int64) string {
	items := make([]string, len(ids))
	for i, id := range ids {
		items[i] = fmt.Sprintf(
			`{"starred_at":"2024-01-01T00:00:00Z","repo":{"id":%d,"full_name":"owner/repo%d"}}`, id, id)
	}
	return "[" + strings.Join(items, ",") + "]"
}

// testRESTClient points a real go-github client at a test server, so the
// paging and decoding under test are the ones that run against GitHub.
func testRESTClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	base := srv.URL + "/"
	api, err := github.NewClient(github.WithURLs(&base, &base))
	if err != nil {
		t.Fatal(err)
	}
	return &Client{api: api}
}

func ids(repos []Repo) []int64 {
	out := make([]int64, len(repos))
	for i, r := range repos {
		out[i] = r.ID
	}
	return out
}

// Topping up a cached star list must read the last cached page onwards and
// nothing before it: that is the whole saving, two requests instead of one per
// hundred stars.
func TestStarredIncrementalReadsOnlyTheTail(t *testing.T) {
	var pages []string
	c := testRESTClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages = append(pages, r.URL.Query().Get("page"))
		// 150 stars are cached, so page 2 holds the last 50 of them plus
		// whatever has been starred since.
		fmt.Fprint(w, starPage([]int64{101, 102, 103, 104, 105, 106, 107, 108, 109, 110,
			111, 112, 113, 114, 115, 116, 117, 118, 119, 120, 121, 122, 123, 124, 125,
			126, 127, 128, 129, 130, 131, 132, 133, 134, 135, 136, 137, 138, 139, 140,
			141, 142, 143, 144, 145, 146, 147, 148, 149, 150, 151, 152}))
	}))

	cached := make([]Repo, 150)
	for i := range cached {
		cached[i] = Repo{ID: int64(i + 1), FullName: fmt.Sprintf("owner/repo%d", i+1)}
	}

	merged, ok, err := c.StarredIncremental(context.Background(), cached)
	if err != nil || !ok {
		t.Fatalf("StarredIncremental = (%v, %v), want a merged list", ok, err)
	}
	if len(merged) != 152 {
		t.Fatalf("merged %d stars, want 152", len(merged))
	}
	got := ids(merged)
	for i, id := range got {
		if id != int64(i+1) {
			t.Fatalf("merged[%d] = %d, want %d; the cached prefix and the fresh tail did not line up", i, id, i+1)
		}
	}
	if len(pages) != 1 || pages[0] != "2" {
		t.Errorf("requested pages %v, want only page 2", pages)
	}
}

// An unstar shifts every later repository down a slot, and an incremental
// fetch that trusted its cached prefix would keep planning around a repository
// the account no longer stars. The overlap is checked for exactly this.
func TestStarredIncrementalDetectsAShiftedList(t *testing.T) {
	c := testRESTClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// One of the first hundred stars was removed, so what was on page 2
		// has moved up by one.
		shifted := make([]int64, 0, 51)
		for id := int64(102); id <= 152; id++ {
			shifted = append(shifted, id)
		}
		fmt.Fprint(w, starPage(shifted))
	}))

	cached := make([]Repo, 150)
	for i := range cached {
		cached[i] = Repo{ID: int64(i + 1)}
	}

	merged, ok, err := c.StarredIncremental(context.Background(), cached)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Errorf("a shifted star list was accepted as a top-up: %d stars", len(merged))
	}
}

// Below one page there is nothing to save, and the caller should not spend a
// request finding that out.
func TestStarredIncrementalSkipsASmallCache(t *testing.T) {
	calls := 0
	c := testRESTClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		fmt.Fprint(w, "[]")
	}))
	if _, ok, err := c.StarredIncremental(context.Background(), make([]Repo, 50)); ok || err != nil {
		t.Errorf("StarredIncremental = (%v, %v), want (false, nil)", ok, err)
	}
	if calls != 0 {
		t.Errorf("%d API calls for a cache too small to top up, want 0", calls)
	}
}
