package gh

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeAPI serves canned GraphQL replies in order, recording what was asked.
type fakeAPI struct {
	replies []string
	queries []string
	vars    []map[string]any
}

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	_ = json.Unmarshal(body, &req)
	f.queries = append(f.queries, req.Query)
	f.vars = append(f.vars, req.Variables)

	reply := `{"data":{}}`
	if len(f.queries) <= len(f.replies) {
		reply = f.replies[len(f.queries)-1]
	}
	_, _ = io.WriteString(w, reply)
}

func testClient(t *testing.T, f *fakeAPI) *ListsClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	c, err := NewListsClient("token", WithHTTPClient(srv.Client()), WithEndpoint(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A list's contents are paged separately from its metadata: asking for fifty
// lists by a hundred items in one query makes GitHub answer 502, because the
// cost limit is roughly the product of the two.
func TestListsPagesItemsSeparately(t *testing.T) {
	f := &fakeAPI{replies: []string{
		`{"data":{"viewer":{"lists":{"pageInfo":{"hasNextPage":false},"nodes":[
			{"id":"L1","name":"Go","slug":"go","description":"d","isPrivate":false,"items":{"totalCount":3}}]}}}}`,
		`{"data":{"node":{"items":{"pageInfo":{"hasNextPage":true,"endCursor":"c1"},
			"nodes":[{"__typename":"Repository","nameWithOwner":"a/one"}]}}}}`,
		`{"data":{"node":{"items":{"pageInfo":{"hasNextPage":false},
			"nodes":[{"__typename":"Repository","nameWithOwner":"b/two"},
			         {"__typename":"User","login":"someone"}]}}}}`,
	}}
	c := testClient(t, f)

	lists, err := c.ListsWithRepos(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(lists) != 1 || lists[0].ID != "L1" || lists[0].Count != 3 {
		t.Fatalf("lists = %+v", lists)
	}
	// Both pages, and only repositories: a list can hold users too, and those
	// are not ours to manage.
	if got := strings.Join(lists[0].Repos, ","); got != "a/one,b/two" {
		t.Errorf("repos = %q, want a/one,b/two", got)
	}
	if f.vars[2]["cursor"] != "c1" {
		t.Errorf("second page did not use the cursor: %v", f.vars[2])
	}
}

// A list with nothing in it must not cost a second request.
func TestListsSkipsEmptyLists(t *testing.T) {
	f := &fakeAPI{replies: []string{
		`{"data":{"viewer":{"lists":{"pageInfo":{"hasNextPage":false},"nodes":[
			{"id":"L1","name":"Empty","items":{"totalCount":0}}]}}}}`,
	}}
	c := testClient(t, f)
	if _, err := c.ListsWithRepos(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.queries) != 1 {
		t.Errorf("made %d requests for an empty list, want 1", len(f.queries))
	}
}

// The outer list connection pages too, for an account near the cap.
func TestListsFollowsListPagination(t *testing.T) {
	f := &fakeAPI{replies: []string{
		`{"data":{"viewer":{"lists":{"pageInfo":{"hasNextPage":true,"endCursor":"p2"},"nodes":[
			{"id":"L1","name":"One","items":{"totalCount":0}}]}}}}`,
		`{"data":{"viewer":{"lists":{"pageInfo":{"hasNextPage":false},"nodes":[
			{"id":"L2","name":"Two","items":{"totalCount":0}}]}}}}`,
	}}
	c := testClient(t, f)
	lists, err := c.Lists(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(lists) != 2 {
		t.Fatalf("got %d lists across two pages, want 2", len(lists))
	}
	if f.vars[1]["listCursor"] != "p2" {
		t.Errorf("second page did not use the cursor: %v", f.vars[1])
	}
}

// A token that can read but not write must say so precisely, because that is
// a thing the user can fix in one command — and the run should stop rather
// than repeat the same refusal per repository.
func TestInsufficientScopesIsRecognised(t *testing.T) {
	f := &fakeAPI{replies: []string{
		`{"errors":[{"type":"INSUFFICIENT_SCOPES","message":"requires user scope"}]}`,
	}}
	c := testClient(t, f)
	err := c.SetItemLists(context.Background(), "R1", []string{"L1"})
	if !errors.Is(err, ErrNeedsUserScope) {
		t.Fatalf("got %v, want ErrNeedsUserScope", err)
	}
}

// GraphQL reports failures in the body with HTTP 200, so a plain status check
// would read an error as success.
func TestGraphQLErrorsAreNotSuccess(t *testing.T) {
	f := &fakeAPI{replies: []string{`{"errors":[{"message":"Name has already been taken"}]}`}}
	c := testClient(t, f)
	if _, err := c.CreateList(context.Background(), "Go", "d", false); err == nil {
		t.Fatal("an errors array was treated as success")
	} else if !strings.Contains(err.Error(), "already been taken") {
		t.Errorf("error lost GitHub's message: %v", err)
	}
}

// Creation must not report success without a list: the caller marks the
// category as existing and would then fail on every repository in it.
func TestCreateListWithoutAListIsAnError(t *testing.T) {
	f := &fakeAPI{replies: []string{`{"data":{"createUserList":{"list":null}}}`}}
	c := testClient(t, f)
	if _, err := c.CreateList(context.Background(), "Go", "d", false); err == nil {
		t.Fatal("creation reported success with no list returned")
	}
}

// The mutation replaces the whole set, so the ids sent are exactly the
// memberships the repository will have afterwards.
func TestSetItemListsSendsTheWholeSet(t *testing.T) {
	f := &fakeAPI{replies: []string{`{"data":{"updateUserListsForItem":{"item":{"__typename":"Repository"}}}}`}}
	c := testClient(t, f)
	if err := c.SetItemLists(context.Background(), "R1", []string{"L1", "L2"}); err != nil {
		t.Fatal(err)
	}
	ids, _ := f.vars[0]["listIds"].([]any)
	if len(ids) != 2 || ids[0] != "L1" || ids[1] != "L2" {
		t.Errorf("sent %v, want [L1 L2]", f.vars[0]["listIds"])
	}
}

// Clearing every membership must send an empty array, not null: null would be
// a type error, and omitting the field would leave the memberships in place.
func TestSetItemListsClearsWithEmptyArray(t *testing.T) {
	f := &fakeAPI{replies: []string{`{"data":{}}`}}
	c := testClient(t, f)
	if err := c.SetItemLists(context.Background(), "R1", nil); err != nil {
		t.Fatal(err)
	}
	ids, ok := f.vars[0]["listIds"].([]any)
	if !ok || ids == nil {
		t.Errorf("listIds = %#v, want an empty array", f.vars[0]["listIds"])
	}
}

// Node ids are resolved in batches rather than one request per repository.
func TestRepoIDsBatches(t *testing.T) {
	f := &fakeAPI{replies: []string{
		`{"data":{"r0":{"id":"ID1","nameWithOwner":"a/one"},"r1":{"id":"ID2","nameWithOwner":"b/two"}}}`,
	}}
	c := testClient(t, f)
	ids, err := c.RepoIDs(context.Background(), []string{"a/one", "b/two"})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.queries) != 1 {
		t.Errorf("made %d requests for two repositories, want 1", len(f.queries))
	}
	if ids["a/one"] != "ID1" || ids["b/two"] != "ID2" {
		t.Errorf("ids = %v", ids)
	}
}

// GitHub answers a batch containing a deleted repository with HTTP 200, the
// other results intact, and a NOT_FOUND alongside them. Treating that as a
// plain failure threw away every good id, so one deleted star aborted an
// entire apply — and --continue could not help, because the failure came
// before the per-repository loop.
func TestRepoIDsKeepsPartialResults(t *testing.T) {
	f := &fakeAPI{replies: []string{`{
		"data":{"r0":{"id":"ID1"},"r1":null,"r2":{"id":"ID3"}},
		"errors":[{"type":"NOT_FOUND","path":["r1"],"message":"Could not resolve to a Repository"}]
	}`}}
	c := testClient(t, f)

	ids, err := c.RepoIDs(context.Background(), []string{"a/one", "gone/away", "c/three"})
	if err != nil {
		t.Fatalf("a deleted repository failed the whole batch: %v", err)
	}
	if ids["a/one"] != "ID1" || ids["c/three"] != "ID3" {
		t.Errorf("good results were discarded: %v", ids)
	}
	if _, ok := ids["gone/away"]; ok {
		t.Errorf("the missing repository got an id: %v", ids)
	}
}

// A repository that has been renamed since it was starred comes back under its
// new name, because repository(owner:name:) follows renames. Keying the result
// by the returned name meant the caller looked it up under the old one and
// never filed it.
func TestRepoIDsKeysByRequestedName(t *testing.T) {
	f := &fakeAPI{replies: []string{`{"data":{"r0":{"id":"ID1"}}}`}}
	c := testClient(t, f)

	ids, err := c.RepoIDs(context.Background(), []string{"facebook/react-native"})
	if err != nil {
		t.Fatal(err)
	}
	if ids["facebook/react-native"] != "ID1" {
		t.Errorf("a renamed repository is unreachable under the name the plan holds: %v", ids)
	}
}

// An error that is not a missing repository must still fail the batch, rather
// than silently returning fewer ids than were asked for.
func TestRepoIDsStillFailsOnRealErrors(t *testing.T) {
	f := &fakeAPI{replies: []string{`{
		"data":{"r0":{"id":"ID1"}},
		"errors":[{"type":"SOMETHING_ELSE","message":"boom"}]
	}`}}
	c := testClient(t, f)
	if _, err := c.RepoIDs(context.Background(), []string{"a/one", "b/two"}); err == nil {
		t.Fatal("an unexpected GraphQL error was ignored")
	}
}

// Exceeding the GraphQL budget arrives as HTTP 200 with the error in the body,
// so the status-code check inside the retry never sees it. It has to be waited
// out, and bounded so an exhausted account fails with a message rather than
// hanging.
func TestRateLimitedGivesUpEventually(t *testing.T) {
	replies := make([]string, maxRateLimitWaits+2)
	for i := range replies {
		replies[i] = `{"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`
	}
	f := &fakeAPI{replies: replies}
	c := testClient(t, f)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the wait honours cancellation, so this returns at once
	_, err := c.Lists(ctx)
	if err == nil {
		t.Fatal("a rate-limited reply was treated as success")
	}
}

// A 502 is what GitHub answers when a query takes too long. It is transient,
// and it is exactly the failure that shaped this client's query splitting, so
// it must be retried rather than ending the run.
func TestServerErrorsAreRetried(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = io.WriteString(w, `{"data":{"viewer":{"login":"octocat"}}}`)
	}))
	t.Cleanup(srv.Close)
	c, err := NewListsClient("token", WithHTTPClient(srv.Client()), WithEndpoint(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	login, err := c.Login(context.Background())
	if err != nil {
		t.Fatalf("a 502 was not retried: %v", err)
	}
	if login != "octocat" || hits != 2 {
		t.Errorf("login=%q after %d attempts", login, hits)
	}
}

// GitHub refuses a document that is too complex with "Resource limits for this
// query exceeded". The ceiling is on complexity, not on the number of
// mutations, so it moves with how many lists each repository is in — no fixed
// batch size is always safe. The client halves and retries until it fits, so
// the caller never has to guess.
func TestOversizedBatchIsSplitAndRetried(t *testing.T) {
	tooBig := `{"errors":[{"type":"MAX_NODE_LIMIT_EXCEEDED","message":"Resource limits for this query exceeded."}]}`
	ok := `{"data":{"m0":{"item":{"__typename":"Repository"}},"m1":{"item":{"__typename":"Repository"}}}}`
	// First attempt (4) is refused, then each half of 2 succeeds.
	f := &fakeAPI{replies: []string{tooBig, ok, ok}}
	c := testClient(t, f)

	items := []ItemLists{
		{RepoID: "R1", ListIDs: []string{"L1"}, Name: "a/one"},
		{RepoID: "R2", ListIDs: []string{"L1"}, Name: "b/two"},
		{RepoID: "R3", ListIDs: []string{"L1"}, Name: "c/three"},
		{RepoID: "R4", ListIDs: []string{"L1"}, Name: "d/four"},
	}
	failed, err := c.SetItemListsBatch(context.Background(), items)
	if err != nil {
		t.Fatalf("an oversized batch was not split: %v", err)
	}
	if len(failed) != 0 {
		t.Errorf("split batches reported failures: %v", failed)
	}
	if len(f.queries) != 3 {
		t.Errorf("made %d requests, want 3 (one refused, two halves)", len(f.queries))
	}
}

// A single item that is still refused has nothing left to split, so the error
// must surface rather than recursing forever.
func TestUnsplittableBatchFails(t *testing.T) {
	tooBig := `{"errors":[{"message":"Resource limits for this query exceeded."}]}`
	f := &fakeAPI{replies: []string{tooBig, tooBig, tooBig}}
	c := testClient(t, f)
	_, err := c.SetItemListsBatch(context.Background(),
		[]ItemLists{{RepoID: "R1", ListIDs: []string{"L1"}, Name: "a/one"}})
	if err == nil {
		t.Fatal("a single unsplittable item reported success")
	}
}

// A per-repository failure inside a batch must be attributed to that
// repository and must not discard the ones that succeeded.
func TestBatchAttributesPerItemFailures(t *testing.T) {
	f := &fakeAPI{replies: []string{`{
		"data":{"m0":{"item":{"__typename":"Repository"}},"m1":null},
		"errors":[{"type":"NOT_FOUND","path":["m1"],"message":"Could not resolve to a node"}]
	}`}}
	c := testClient(t, f)
	failed, err := c.SetItemListsBatch(context.Background(), []ItemLists{
		{RepoID: "R1", ListIDs: []string{"L1"}, Name: "a/one"},
		{RepoID: "R2", ListIDs: []string{"L1"}, Name: "b/two"},
	})
	if err != nil {
		t.Fatalf("one bad item failed the whole batch: %v", err)
	}
	if _, bad := failed["b/two"]; !bad {
		t.Errorf("the failing repository was not reported: %v", failed)
	}
	if _, bad := failed["a/one"]; bad {
		t.Errorf("a successful repository was reported as failed: %v", failed)
	}
}

// Deleting a list with a few hundred members is enough work that GitHub
// refuses it with "Resource limits for this query exceeded". Unlike a batched
// write there is nothing to split, so the only remedy is to ask again — and a
// failed deletion leaves the account over its list cap, which aborts the run.
func TestDeleteListRetriesWhenRefusedAsTooLarge(t *testing.T) {
	f := &fakeAPI{replies: []string{
		`{"errors":[{"message":"Resource limits for this query exceeded."}]}`,
		`{"data":{"deleteUserList":{"user":{"id":"u"}}}}`,
	}}
	c := testClient(t, f)
	if err := c.DeleteList(context.Background(), "L1"); err != nil {
		t.Fatalf("a refused deletion was not retried: %v", err)
	}
	if len(f.queries) != 2 {
		t.Errorf("made %d attempts, want 2", len(f.queries))
	}
}

// It must still give up rather than retry forever.
func TestDeleteListGivesUpEventually(t *testing.T) {
	replies := make([]string, maxTooLargeRetries+2)
	for i := range replies {
		replies[i] = `{"errors":[{"message":"Resource limits for this query exceeded."}]}`
	}
	f := &fakeAPI{replies: replies}
	c := testClient(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the wait honours cancellation, so this returns promptly
	if err := c.DeleteList(ctx, "L1"); err == nil {
		t.Fatal("a persistently refused deletion reported success")
	}
}

// Deleting a list that is already gone is the outcome asked for, not a
// failure. GitHub answers a stale id with NOT_FOUND, and treating that as an
// error aborted a reset partway through with most of the work already done.
func TestDeleteListTreatsMissingAsDone(t *testing.T) {
	f := &fakeAPI{replies: []string{
		`{"data":{"deleteUserList":null},"errors":[{"type":"NOT_FOUND","message":"Could not resolve to a node"}]}`,
	}}
	c := testClient(t, f)
	if err := c.DeleteList(context.Background(), "L-gone"); err != nil {
		t.Fatalf("deleting an absent list reported failure: %v", err)
	}
}

// A different error must still fail, or a reset would report success while
// leaving lists behind.
func TestDeleteListStillFailsOnRealErrors(t *testing.T) {
	f := &fakeAPI{replies: []string{`{"errors":[{"type":"FORBIDDEN","message":"nope"}]}`}}
	c := testClient(t, f)
	if err := c.DeleteList(context.Background(), "L1"); err == nil {
		t.Fatal("a real error was swallowed")
	}
}
