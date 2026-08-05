package ledger

import "context"

// ProjectSummary is a top-level status rollup for a project: counts by
// status, and the items that most need attention right now. Computed by
// tallying ListItems in Go rather than a dedicated SQL aggregation query --
// ledger-scale item counts (dozens, not millions) make that the simpler,
// equally-fast choice.
type ProjectSummary struct {
	ProjectKey  string
	Counts      map[string]int // status -> count
	InProgress  []Item
	Blocked     []Item
	TopPriority *Item // highest-priority item not yet done; nil if none or none have a priority set
}

// SummarizeProject computes a ProjectSummary for projectID.
func (db *DB) SummarizeProject(ctx context.Context, projectID, projectKey string) (*ProjectSummary, error) {
	items, err := db.ListItems(ctx, ItemFilter{ProjectID: projectID})
	if err != nil {
		return nil, err
	}

	sum := &ProjectSummary{ProjectKey: projectKey, Counts: map[string]int{}}
	for i := range items {
		it := items[i]
		sum.Counts[it.Status]++

		switch it.Status {
		case "in_progress":
			sum.InProgress = append(sum.InProgress, it)
		case "blocked":
			sum.Blocked = append(sum.Blocked, it)
		}

		if it.Status != "done" && it.Priority.Valid {
			if sum.TopPriority == nil || it.Priority.Int64 > sum.TopPriority.Priority.Int64 {
				itCopy := it
				sum.TopPriority = &itCopy
			}
		}
	}
	return sum, nil
}
