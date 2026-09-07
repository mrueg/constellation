// Package plan turns a clustering into a reviewable, replayable description of
// the star lists to create and the repositories to file under them.
package plan

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mrueg/constellation/internal/cluster"
	"github.com/mrueg/constellation/internal/embed"
	"github.com/mrueg/constellation/internal/gh"
	"github.com/mrueg/constellation/internal/ui"
)

// Plan is the full proposal. It is written to disk so that the categorization
// a human reviewed is exactly the one that later gets applied.
type Plan struct {
	Version     int       `json:"version"`
	GeneratedAt time.Time `json:"generated_at"`
	User        string    `json:"user"`
	Backend     string    `json:"backend"`
	// Settings records the tuning the plan was produced with, so a plan is
	// self-describing: the categories depend on a dozen weights and
	// thresholds, and without them a plan cannot be reproduced or explained
	// months later except by remembering the command line.
	Settings   map[string]string `json:"settings,omitempty"`
	TotalStars int               `json:"total_stars"`
	Categories []Category        `json:"categories"`
	Unassigned []Repo            `json:"unassigned"`
}

// Category is one proposed or existing star list.
type Category struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Terms       []string `json:"terms"`
	// Exists is true when a list of this name is already on the account, in
	// which case applying only adds repositories to it.
	Exists   bool    `json:"exists"`
	Cohesion float64 `json:"cohesion"`
	Repos    []Repo  `json:"repos"`
}

// Repo is a repository placed in a category.
//
// Stars and the two dates are carried through because the plan is also read by
// people and by scripts: they are what you sort an index by, and what tells a
// dormant project from a live one. They cost nothing to record — every one of
// them was already fetched with the star list.
type Repo struct {
	FullName    string    `json:"full_name"`
	Description string    `json:"description,omitempty"`
	Similarity  float64   `json:"similarity"`
	Stars       int       `json:"stars,omitempty"`
	PushedAt    time.Time `json:"pushed_at,omitempty"`
	StarredAt   time.Time `json:"starred_at,omitempty"`
}

// entry copies the parts of a repository the plan keeps.
func entry(r gh.Repo, similarity float64) Repo {
	return Repo{
		FullName:    r.FullName,
		Description: r.Description,
		Similarity:  round(similarity),
		Stars:       r.Stars,
		PushedAt:    r.PushedAt,
		StarredAt:   r.StarredAt,
	}
}

const version = 1

// Build assembles the plan from the clustering result. Repositories the
// clusterer refused to place are reported rather than hidden: an honest
// "these 40 stars fit no category" is more useful than 40 bad placements.
func Build(user string, repos []gh.Repo, sp *embed.Space, res *cluster.Result, labels []cluster.Label, existing []gh.List) *Plan {
	byName := map[string]bool{}
	for _, l := range existing {
		byName[strings.ToLower(l.Name)] = true
	}

	p := &Plan{
		Version:    version,
		User:       user,
		Backend:    sp.Backend,
		TotalStars: len(repos),
	}
	for c := 0; c < res.K; c++ {
		members := res.Members(c)
		if len(members) == 0 {
			continue
		}
		sort.SliceStable(members, func(i, j int) bool {
			return sp.Rows[members[i]].Dot(res.Centroids[c]) > sp.Rows[members[j]].Dot(res.Centroids[c])
		})
		cat := Category{
			Name:   labels[c].Name,
			Terms:  labels[c].TopTerms,
			Exists: byName[strings.ToLower(labels[c].Name)],
		}
		var sum float64
		for _, m := range members {
			sim := sp.Rows[m].Dot(res.Centroids[c])
			sum += sim
			cat.Repos = append(cat.Repos, entry(repos[m], sim))
		}
		cat.Cohesion = round(sum / float64(len(members)))
		cat.Description = cluster.Describe(labels[c])
		p.Categories = append(p.Categories, cat)
	}
	for i, c := range res.Assign {
		if c < 0 {
			p.Unassigned = append(p.Unassigned, entry(repos[i], 0))
		}
	}
	return p
}

// Stamp records when the plan was produced. It is separate from Build so that
// clustering stays a pure function of its inputs.
func (p *Plan) Stamp(t time.Time) { p.GeneratedAt = t }

// Save writes the plan as JSON, creating parent directories as needed.
func (p *Plan) Save(path string) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// Load reads a plan written by Save.
func Load(path string) (*Plan, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p Plan
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if p.Version != version {
		return nil, fmt.Errorf("%s was written by an incompatible version (%d, want %d); re-run `constellation plan`", path, p.Version, version)
	}
	return &p, nil
}

// Categorized counts the distinct repositories the plan places in at least one
// list.
func (p *Plan) Categorized() int {
	seen := map[string]bool{}
	for _, c := range p.Categories {
		for _, r := range c.Repos {
			seen[r.FullName] = true
		}
	}
	return len(seen)
}

// Placements counts repository-to-list pairs, which is what applying the plan
// actually writes. It exceeds Categorized when repositories belong to more
// than one list.
func (p *Plan) Placements() int {
	n := 0
	for _, c := range p.Categories {
		n += len(c.Repos)
	}
	return n
}

// NewLists counts the categories that do not already exist on the account,
// which is what applying the plan would have to create.
func (p *Plan) NewLists() int {
	n := 0
	for _, c := range p.Categories {
		if !c.Exists && len(c.Repos) > 0 {
			n++
		}
	}
	return n
}

// Print renders the plan for a terminal.
// Print writes the whole plan.
func (p *Plan) Print(w io.Writer, verbose bool) { p.PrintN(w, verbose, 0) }

// PrintN writes the plan, showing at most limit categories; 0 shows all. The
// plan file is unaffected — this only bounds what reaches the terminal, since a
// thirty-category plan scrolls its own summary off the screen.
func (p *Plan) PrintN(w io.Writer, verbose bool, limit int) {
	fmt.Fprintf(w, "\n%s stars → %s lists (%d categorized, %d left alone) using %s\n",
		ui.Title(fmt.Sprint(p.TotalStars)), ui.Title(fmt.Sprint(len(p.Categories))),
		p.Categorized(), len(p.Unassigned), ui.Muted(p.Backend))
	if extra := p.Placements() - p.Categorized(); extra > 0 {
		fmt.Fprintf(w, "%d repositories appear in more than one list (%d placements in total)\n",
			extra, p.Placements())
	}
	fmt.Fprintln(w)
	shownCats := p.Categories
	if limit > 0 && len(shownCats) > limit {
		shownCats = shownCats[:limit]
	}
	for _, c := range shownCats {
		status := "new"
		if c.Exists {
			status = "exists"
		}
		// Padding is computed on the plain name: the escape sequences have no
		// width on screen but do count in a %-34s field, which would leave the
		// columns ragged.
		fmt.Fprintf(w, "  %s%s %3d repos  %s  (%s)\n",
			ui.Title(c.Name), ui.Pad(c.Name, 34),
			len(c.Repos), ui.Muted(fmt.Sprintf("cohesion %.2f", c.Cohesion)), statusColour(status))
		fmt.Fprintf(w, "  %-34s %s\n", "", ui.Muted(strings.Join(c.Terms, ", ")))
		shown := c.Repos
		if !verbose && len(shown) > 5 {
			shown = shown[:5]
		}
		for _, r := range shown {
			fmt.Fprintf(w, "      %-44s %.2f\n", r.FullName, r.Similarity)
		}
		if len(shown) < len(c.Repos) {
			fmt.Fprintf(w, "      … and %d more\n", len(c.Repos)-len(shown))
		}
		fmt.Fprintln(w)
	}
	if n := len(p.Categories) - len(shownCats); n > 0 {
		fmt.Fprintf(w, "  %s\n\n", ui.Muted(fmt.Sprintf("… and %d more categories", n)))
	}
	if len(p.Unassigned) > 0 {
		fmt.Fprintf(w, "  %d stars fit no category well enough to place:\n", len(p.Unassigned))
		shown := p.Unassigned
		if !verbose && len(shown) > 10 {
			shown = shown[:10]
		}
		for _, r := range shown {
			fmt.Fprintf(w, "      %s\n", r.FullName)
		}
		if len(shown) < len(p.Unassigned) {
			fmt.Fprintf(w, "      … and %d more\n", len(p.Unassigned)-len(shown))
		}
		fmt.Fprintln(w)
	}
}

// Markdown renders the plan as a document, useful as a starred-repos index
// even if the lists are never applied.
func (p *Plan) Markdown(w io.Writer) {
	fmt.Fprintf(w, "# %s's stars by category\n\n", p.User)
	fmt.Fprintf(w, "%d starred repositories grouped into %d categories by `constellation` (%s).\n\n",
		p.TotalStars, len(p.Categories), p.Backend)
	for _, c := range p.Categories {
		fmt.Fprintf(w, "## %s (%d)\n\n%s\n\n", c.Name, len(c.Repos), c.Description)
		for _, r := range c.Repos {
			fmt.Fprintf(w, "- [%s](https://github.com/%s)%s", r.FullName, r.FullName, stars(r.Stars))
			if r.Description != "" {
				fmt.Fprintf(w, " — %s", oneLine(r.Description))
			}
			fmt.Fprintln(w)
		}
		fmt.Fprintln(w)
	}
	if len(p.Unassigned) > 0 {
		fmt.Fprintf(w, "## Uncategorized (%d)\n\n", len(p.Unassigned))
		for _, r := range p.Unassigned {
			fmt.Fprintf(w, "- [%s](https://github.com/%s)%s\n", r.FullName, r.FullName, stars(r.Stars))
		}
		fmt.Fprintln(w)
	}
}

// stars renders a stargazer count for the index, thousands abbreviated: an
// index is read down the left edge, and "1.2k" keeps the descriptions from
// being pushed out of line by five-digit numbers.
func stars(n int) string {
	switch {
	case n <= 0:
		return ""
	case n < 1000:
		return fmt.Sprintf(" `★%d`", n)
	default:
		return fmt.Sprintf(" `★%.1fk`", float64(n)/1000)
	}
}

func oneLine(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", " "))
}

func round(f float64) float64 {
	return float64(int(f*1000+0.5)) / 1000
}

func statusColour(status string) string {
	if status == "new" {
		return ui.Added(status)
	}
	return ui.Muted(status)
}
