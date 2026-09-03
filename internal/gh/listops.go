package gh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// MaxLists is how many star lists GitHub allows per account. It is not in the
// documentation, so it is a default rather than a certainty: createUserList
// failing is the authoritative answer.
const MaxLists = 32

// List is a star list and, when read through Lists, everything in it.
type List struct {
	ID          string
	Name        string
	Slug        string
	Description string
	Private     bool
	// Count is how many items the list holds, which is known without reading
	// them.
	Count int
	// Repos holds "owner/name" for every repository in the list. It is filled
	// in only by ListsWithRepos; a listing does not need it and paging every
	// list's contents is by far the slowest thing this client does.
	Repos []string
}

// listsQuery asks only for metadata. Requesting the items inline as well is
// what a first version did, and GitHub answers a query of fifty lists by a
// hundred items with a 502: the cost limit is roughly lists x items <= 500.
// Metadata alone is cheap and predictable, and the contents are then paged per
// list.
const listsQuery = `
query($listCursor: String) {
  viewer {
    lists(first: 50, after: $listCursor) {
      pageInfo { hasNextPage endCursor }
      nodes { id name slug description isPrivate items(first: 1) { totalCount } }
    }
  }
}`

// listItemsQuery pages one list's contents. There is no field addressing a
// list by slug, but UserList is a Node, so its id gives direct access — which
// also means the cursor belongs to one list rather than to a position in the
// outer connection.
const listItemsQuery = `
query($id: ID!, $cursor: String) {
  node(id: $id) {
    ... on UserList {
      items(first: 100, after: $cursor) {
        pageInfo { hasNextPage endCursor }
        nodes { __typename ... on Repository { nameWithOwner } }
      }
    }
  }
}`

type itemsPage struct {
	TotalCount int `json:"totalCount"`
	PageInfo   struct {
		HasNextPage bool   `json:"hasNextPage"`
		EndCursor   string `json:"endCursor"`
	} `json:"pageInfo"`
	Nodes []struct {
		TypeName      string `json:"__typename"`
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"nodes"`
}

func (p itemsPage) names() []string {
	out := make([]string, 0, len(p.Nodes))
	for _, n := range p.Nodes {
		// A list can hold users and organizations too; only repositories are
		// ours to manage.
		if n.TypeName == "Repository" && n.NameWithOwner != "" {
			out = append(out, n.NameWithOwner)
		}
	}
	return out
}

// ListsWithRepos returns every star list together with its full membership.
func (c *ListsClient) ListsWithRepos(ctx context.Context) ([]List, error) {
	lists, err := c.Lists(ctx)
	if err != nil {
		return nil, err
	}
	for i := range lists {
		if lists[i].Count == 0 {
			continue
		}
		repos, err := c.ListItems(ctx, lists[i].ID)
		if err != nil {
			return nil, fmt.Errorf("reading the contents of %q: %w", lists[i].Name, err)
		}
		lists[i].Repos = repos
	}
	return lists, nil
}

// Lists returns every star list on the account, without reading its contents.
func (c *ListsClient) Lists(ctx context.Context) ([]List, error) {
	var out []List
	cursor := ""
	for {
		var reply struct {
			Viewer struct {
				Lists struct {
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
					Nodes []struct {
						ID          string    `json:"id"`
						Name        string    `json:"name"`
						Slug        string    `json:"slug"`
						Description string    `json:"description"`
						IsPrivate   bool      `json:"isPrivate"`
						Items       itemsPage `json:"items"`
					} `json:"nodes"`
				} `json:"lists"`
			} `json:"viewer"`
		}
		vars := map[string]any{"listCursor": nil}
		if cursor != "" {
			vars["listCursor"] = cursor
		}
		if err := c.query(ctx, listsQuery, vars, &reply); err != nil {
			return nil, fmt.Errorf("reading star lists: %w", err)
		}
		for _, n := range reply.Viewer.Lists.Nodes {
			out = append(out, List{
				ID: n.ID, Name: n.Name, Slug: n.Slug,
				Description: n.Description, Private: n.IsPrivate,
				Count: n.Items.TotalCount,
			})
		}
		if !reply.Viewer.Lists.PageInfo.HasNextPage {
			return out, nil
		}
		cursor = reply.Viewer.Lists.PageInfo.EndCursor
	}
}

// ListItems pages one list's contents.
func (c *ListsClient) ListItems(ctx context.Context, id string) ([]string, error) {
	var out []string
	cursor := ""
	for {
		var reply struct {
			Node struct {
				Items itemsPage `json:"items"`
			} `json:"node"`
		}
		vars := map[string]any{"id": id, "cursor": nil}
		if cursor != "" {
			vars["cursor"] = cursor
		}
		if err := c.query(ctx, listItemsQuery, vars, &reply); err != nil {
			return nil, err
		}
		page := reply.Node.Items
		out = append(out, page.names()...)
		if !page.PageInfo.HasNextPage {
			return out, nil
		}
		cursor = page.PageInfo.EndCursor
	}
}

// Login reports the account the token belongs to.
func (c *ListsClient) Login(ctx context.Context) (string, error) {
	var reply struct {
		Viewer struct {
			Login string `json:"login"`
		} `json:"viewer"`
	}
	if err := c.query(ctx, `{ viewer { login } }`, nil, &reply); err != nil {
		return "", err
	}
	return reply.Viewer.Login, nil
}

// CreateList creates a list and returns it.
func (c *ListsClient) CreateList(ctx context.Context, name, description string, private bool) (List, error) {
	var reply struct {
		CreateUserList struct {
			List struct {
				ID   string `json:"id"`
				Name string `json:"name"`
				Slug string `json:"slug"`
			} `json:"list"`
		} `json:"createUserList"`
	}
	const q = `
mutation($name: String!, $description: String!, $isPrivate: Boolean!) {
  createUserList(input: {name: $name, description: $description, isPrivate: $isPrivate}) {
    list { id name slug }
  }
}`
	vars := map[string]any{"name": name, "description": description, "isPrivate": private}
	if err := c.query(ctx, q, vars, &reply); err != nil {
		return List{}, fmt.Errorf("creating list %q: %w", name, err)
	}
	l := reply.CreateUserList.List
	if l.ID == "" {
		return List{}, fmt.Errorf("creating list %q: GitHub returned no list", name)
	}
	return List{ID: l.ID, Name: l.Name, Slug: l.Slug, Description: description, Private: private}, nil
}

// UpdateList sets a list's name, description and visibility.
func (c *ListsClient) UpdateList(ctx context.Context, id, name, description string, private bool) error {
	const q = `
mutation($listId: ID!, $name: String!, $description: String!, $isPrivate: Boolean!) {
  updateUserList(input: {listId: $listId, name: $name, description: $description, isPrivate: $isPrivate}) {
    list { id }
  }
}`
	vars := map[string]any{"listId": id, "name": name, "description": description, "isPrivate": private}
	if err := c.query(ctx, q, vars, nil); err != nil {
		return fmt.Errorf("updating list %q: %w", name, err)
	}
	return nil
}

// DeleteList removes a list. The repositories in it stay starred.
//
// Deleting is retried when GitHub answers "Resource limits for this query
// exceeded": removing a list with a few hundred members is enough work for the
// request to be refused, and unlike a batched write there is nothing here to
// split, so the only remedy is to ask again.
func (c *ListsClient) DeleteList(ctx context.Context, id string) error {
	const q = `
mutation($listId: ID!) {
  deleteUserList(input: {listId: $listId}) { user { id } }
}`
	err := c.retryTooLarge(ctx, func() error {
		errs, err := c.queryPartial(ctx, q, map[string]any{"listId": id}, nil)
		if err != nil {
			return err
		}
		for _, e := range errs {
			// A list that is already gone is the outcome asked for. GitHub
			// answers a stale id with NOT_FOUND, and treating that as a
			// failure made a reset abort partway through with most of the
			// work done and no way to finish it except running again.
			if e.Type == "NOT_FOUND" {
				return nil
			}
		}
		if len(errs) > 0 {
			return graphQLFailure(errs)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("deleting list: %w", err)
	}
	return nil
}

// maxTooLargeRetries bounds how often an operation that cannot be made smaller
// is re-attempted after GitHub refuses it for being too much work.
const maxTooLargeRetries = 4

func (c *ListsClient) retryTooLarge(ctx context.Context, op func() error) error {
	var err error
	for attempt := range maxTooLargeRetries {
		if err = op(); !errors.Is(err, errQueryTooLarge) {
			return err
		}
		wait := time.Duration(attempt+1) * 2 * time.Second
		c.logf("GitHub refused the request as too large, retrying in %s", wait)
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

// SetItemLists sets the complete set of lists a repository belongs to.
//
// The mutation replaces the whole set, exactly as the web UI's dialog did, so
// callers must pass the memberships they want to keep as well as the one they
// are adding — otherwise a hand-curated list silently loses the repository.
func (c *ListsClient) SetItemLists(ctx context.Context, repoID string, listIDs []string) error {
	const q = `
mutation($itemId: ID!, $listIds: [ID!]!) {
  updateUserListsForItem(input: {itemId: $itemId, listIds: $listIds}) {
    item { __typename }
  }
}`
	ids := listIDs
	if ids == nil {
		ids = []string{}
	}
	if err := c.query(ctx, q, map[string]any{"itemId": repoID, "listIds": ids}, nil); err != nil {
		return fmt.Errorf("setting list membership: %w", err)
	}
	return nil
}

// RepoIDs resolves "owner/name" to the node ids the mutations take, in batches
// of one aliased query rather than one request each.
func (c *ListsClient) RepoIDs(ctx context.Context, fullNames []string) (map[string]string, error) {
	const batch = 100
	out := make(map[string]string, len(fullNames))
	for start := 0; start < len(fullNames); start += batch {
		end := min(start+batch, len(fullNames))
		chunk := fullNames[start:end]

		// The alias remembers which name was asked for. Keying the result by
		// the name GitHub returns instead looks equivalent but is not:
		// repository(owner:name:) follows renames, so a starred repository
		// that has since moved comes back under its new name, the caller
		// looks it up under the old one, and it is never filed.
		asked := map[string]string{}
		var b strings.Builder
		b.WriteString("query {\n")
		for i, full := range chunk {
			owner, name, ok := strings.Cut(full, "/")
			if !ok {
				continue
			}
			alias := fmt.Sprintf("r%d", i)
			asked[alias] = full
			fmt.Fprintf(&b, "  %s: repository(owner: %q, name: %q) { id }\n", alias, owner, name)
		}
		b.WriteString("}")
		if len(asked) == 0 {
			continue
		}

		var reply map[string]struct {
			ID string `json:"id"`
		}
		// Partial answers are normal here: a repository that has been deleted
		// or made private since it was starred comes back as null alongside a
		// NOT_FOUND, and the other ninety-nine results are still good.
		errs, err := c.queryPartial(ctx, b.String(), nil, &reply)
		if err != nil {
			return nil, fmt.Errorf("resolving repository ids: %w", err)
		}
		for _, e := range errs {
			if e.Type != "NOT_FOUND" {
				return nil, fmt.Errorf("resolving repository ids: %w", graphQLFailure(errs))
			}
		}
		for alias, v := range reply {
			if v.ID != "" {
				out[asked[alias]] = v.ID
			}
		}
	}
	return out, nil
}

// ItemLists is one repository and the complete set of lists it should belong
// to afterwards.
type ItemLists struct {
	RepoID  string
	ListIDs []string
	// Name is carried only so failures can be reported against something the
	// user recognises.
	Name string
}

// SetItemListsBatch applies several membership changes in one request.
//
// GraphQL executes top-level mutation fields serially and in order, so a batch
// behaves exactly like the same mutations sent one at a time — but it costs one
// request instead of one per repository. That matters because GitHub's limit on
// mutating requests is the binding constraint on how long an apply takes.
//
// Failures are per repository: GitHub answers with the successes intact and an
// error naming the alias that failed, so a batch is not all-or-nothing. The
// returned map is keyed by repository name.
func (c *ListsClient) SetItemListsBatch(ctx context.Context, items []ItemLists) (map[string]error, error) {
	if len(items) == 0 {
		return nil, nil
	}

	failed, err := c.setItemListsOnce(ctx, items)
	if err == nil {
		return failed, nil
	}
	// GitHub rejects a document that is too complex with "Resource limits for
	// this query exceeded". The ceiling is on complexity, not on the number of
	// mutations, so it moves with how many lists each repository belongs to —
	// which means no fixed batch size is always safe. Halving and retrying
	// finds a size that fits without the caller having to guess.
	if !errors.Is(err, errQueryTooLarge) || len(items) == 1 {
		return nil, err
	}
	c.logf("batch of %d was too large for one request, splitting", len(items))
	mid := len(items) / 2
	left, err := c.SetItemListsBatch(ctx, items[:mid])
	if err != nil {
		return nil, err
	}
	right, err := c.SetItemListsBatch(ctx, items[mid:])
	if err != nil {
		return nil, err
	}
	out := map[string]error{}
	for k, v := range left {
		out[k] = v
	}
	for k, v := range right {
		out[k] = v
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// errQueryTooLarge marks GitHub refusing a document for being too much work,
// either because it asks for too many things at once or because the one thing
// it asks for is large.
var errQueryTooLarge = errors.New("graphql query exceeded GitHub's resource limits")

func (c *ListsClient) setItemListsOnce(ctx context.Context, items []ItemLists) (map[string]error, error) {

	var decl, body strings.Builder
	vars := map[string]any{}
	alias := make([]string, len(items))
	for i, it := range items {
		a := fmt.Sprintf("m%d", i)
		alias[i] = a
		if i > 0 {
			decl.WriteString(", ")
		}
		fmt.Fprintf(&decl, "$i%d: ID!, $l%d: [ID!]!", i, i)
		fmt.Fprintf(&body, "  %s: updateUserListsForItem(input: {itemId: $i%d, listIds: $l%d}) { item { __typename } }\n", a, i, i)
		ids := it.ListIDs
		if ids == nil {
			ids = []string{}
		}
		vars[fmt.Sprintf("i%d", i)] = it.RepoID
		vars[fmt.Sprintf("l%d", i)] = ids
	}
	query := fmt.Sprintf("mutation(%s) {\n%s}", decl.String(), body.String())

	var reply map[string]json.RawMessage
	errs, err := c.queryPartial(ctx, query, vars, &reply)
	if err != nil {
		return nil, err
	}

	// An error carries the alias that produced it in its path, which is how a
	// partial failure is attributed to the right repository.
	byAlias := map[string]error{}
	for _, e := range errs {
		if strings.Contains(e.Message, "Resource limits") {
			return nil, errQueryTooLarge
		}
		if len(e.Path) == 0 {
			// Not attributable to one field, so the whole batch is suspect.
			return nil, graphQLFailure(errs)
		}
		if a, ok := e.Path[0].(string); ok {
			byAlias[a] = fmt.Errorf("%s", e.Message)
		}
	}
	if len(byAlias) == 0 {
		return nil, nil
	}
	failed := map[string]error{}
	for i, it := range items {
		if e, bad := byAlias[alias[i]]; bad {
			failed[it.Name] = e
		}
	}
	return failed, nil
}
