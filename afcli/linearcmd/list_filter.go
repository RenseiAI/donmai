package linearcmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/internal/linear"
)

const maxListFilterBytes = 64 * 1024

// listFilterFlags supplies data for the existing list operation, never a query.
type listFilterFlags struct {
	inline          string
	file            string
	includeArchived bool
}

func (f *listFilterFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.inline, "filter", "", "GraphQL filter JSON object (max 64 KiB); ANDed with named filters; API validates fields")
	cmd.Flags().StringVar(&f.file, "filter-file", "", "Read the GraphQL filter JSON object from a file (max 64 KiB); exclusive with --filter")
	cmd.Flags().BoolVar(&f.includeArchived, "include-archived", false, "Include archived results using the API pagination option")
}

func (f *listFilterFlags) parse(cmd *cobra.Command) (map[string]any, error) {
	inlineSet, fileSet := cmd.Flags().Changed("filter"), cmd.Flags().Changed("filter-file")
	if inlineSet && fileSet {
		return nil, fmt.Errorf("--filter and --filter-file are mutually exclusive")
	}
	if !inlineSet && !fileSet {
		return nil, nil
	}
	var reader io.Reader = strings.NewReader(f.inline)
	if fileSet {
		file, err := os.Open(f.file) //nolint:gosec // user-selected filter file
		if err != nil {
			return nil, fmt.Errorf("open filter file: %w", err)
		}
		defer func() { _ = file.Close() }()
		reader = file
	}
	data, err := io.ReadAll(io.LimitReader(reader, maxListFilterBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read filter: %w", err)
	}
	if len(data) > maxListFilterBytes {
		return nil, fmt.Errorf("filter exceeds 64 KiB")
	}
	var filter map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	if err := decoder.Decode(&filter); err != nil {
		return nil, fmt.Errorf("filter must be a JSON object: %w", err)
	}
	if filter == nil {
		return nil, fmt.Errorf("filter must be a JSON object, not null")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, fmt.Errorf("filter must contain exactly one JSON object")
	}
	return filter, nil
}

// Keep the original Linear contract available to other consumers and mocks.
// The listing commands use additive methods on the same selected transport.
type filteredListClient interface {
	linear.Linear
	ListIssuesWithOptions(context.Context, map[string]any, int, string, bool) (*linear.IssueListResult, error)
	ListProjectsWithOptions(context.Context, string, map[string]any, bool) ([]linear.Project, error)
}

func newFilteredListClient(ds func() afclient.DataSource, bin string) (filteredListClient, error) {
	client, err := newLinearClient(ds, bin)
	if err != nil {
		return nil, err
	}
	lister, ok := client.(filteredListClient)
	if !ok {
		return nil, fmt.Errorf("structured listing is unavailable from this Linear client")
	}
	return lister, nil
}
