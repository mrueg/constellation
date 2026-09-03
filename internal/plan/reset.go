package plan

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/mrueg/constellation/internal/cluster"
	"github.com/mrueg/constellation/internal/gh"
	"github.com/mrueg/constellation/internal/ui"
)

// ResetOptions control how Reset tears down the lists this tool created.
type ResetOptions struct {
	DryRun          bool
	ContinueOnError bool
	Delay           time.Duration
	Out             io.Writer
}

// ResetResult reports what Reset did, or would do.
type ResetResult struct {
	Deleted []string
	// Kept are lists left alone because they were not created here.
	Kept   []string
	Errors []error
}

// Reset deletes every star list this tool created, identified by the marker its
// descriptions carry, and leaves every other list untouched. It is the inverse
// of apply: a way back to a clean account after experimenting, without deleting
// anything the user made by hand.
func Reset(ctx context.Context, c *gh.ListsClient, user string, opt ResetOptions) (*ResetResult, error) {
	out := opt.Out
	if out == nil {
		out = io.Discard
	}
	res := &ResetResult{}

	lists, err := c.Lists(ctx)
	if err != nil {
		return nil, err
	}

	for _, l := range lists {
		if !strings.Contains(l.Description, cluster.DescriptionMarker) {
			res.Kept = append(res.Kept, l.Name)
			fmt.Fprintf(out, "%s %s; it was not created here\n", ui.Info("keeping"), ui.Name(l.Name))
			continue
		}
		if opt.DryRun {
			fmt.Fprintf(out, "would delete %s\n", ui.Name(l.Name))
			res.Deleted = append(res.Deleted, l.Name)
			continue
		}
		if err := c.DeleteList(ctx, l.ID); err != nil {
			if !opt.ContinueOnError || terminal(ctx, err) {
				return res, fmt.Errorf("deleting list %q: %w", l.Name, err)
			}
			res.Errors = append(res.Errors, err)
			continue
		}
		res.Deleted = append(res.Deleted, l.Name)
		fmt.Fprintf(out, "%s %s\n", ui.Removed("deleted"), ui.Name(l.Name))
		pause(ctx, opt.Delay)
	}
	return res, nil
}
