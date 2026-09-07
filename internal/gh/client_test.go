package gh

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-github/v90/github"
)

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
