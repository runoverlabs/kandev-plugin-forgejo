package watches

import (
	"context"
	"fmt"
	"strings"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

// maxTaskPages bounds the inflight scan. A workspace with more live tasks than
// this is past the point where a watch's budget is the interesting constraint.
const maxTaskPages = 20

// taskPageSize is the page size for the inflight scan.
const taskPageSize = 200

// inflight counts this watch's tasks that are still open.
//
// It reads the ledger, then one page-walk of the workspace's live tasks, and
// intersects them — rather than a Get per recorded task, which would be one
// RPC per issue this watch has ever handled. A task kandev has completed or
// archived no longer counts against the budget, which is what makes the budget
// a throttle rather than a lifetime cap.
func (p *Poller) inflight(ctx context.Context, host pluginsdk.Host, watch Watch) (int, error) {
	records, err := p.store.Records(ctx, watch)
	if err != nil {
		return 0, err
	}
	if len(records) == 0 {
		return 0, nil
	}
	owned := make(map[string]struct{}, len(records))
	for _, record := range records {
		owned[record.TaskID] = struct{}{}
	}

	count := 0
	err = walkTasks(ctx, host, pluginsdk.TaskFilter{
		WorkspaceIDs: []string{watch.WorkspaceID},
	}, func(task pluginsdk.Task) {
		if _, mine := owned[task.ID]; mine && occupiesBoard(watch, task) {
			count++
		}
	})
	if err != nil {
		return 0, fmt.Errorf("watches: list tasks: %w", err)
	}
	return count, nil
}

// occupiesBoard reports whether a task still counts against its watch's budget.
//
// CompletedAt and ArchivedAt are the two fields that mean "no longer occupying
// the board". Task state vocabulary is richer than this plugin needs to know
// about.
//
// A review watch also counts the state: it ends a task by marking it COMPLETED
// when it may not archive, and that sets no completed_at, so the budget would
// otherwise never free. Issue watches keep reading the timestamps alone, as
// they always have.
func occupiesBoard(watch Watch, task pluginsdk.Task) bool {
	return task.CompletedAt == nil && task.ArchivedAt == nil && !(watch.IsReview() && taskDone(task))
}

// walkTasks pages through the tasks filter selects, up to maxTaskPages, and
// hands each to visit.
func walkTasks(ctx context.Context, host pluginsdk.Host, filter pluginsdk.TaskFilter, visit func(pluginsdk.Task)) error {
	cursor := ""
	for page := 0; page < maxTaskPages; page++ {
		tasks, info, err := host.Tasks().List(ctx, filter, pluginsdk.Page{Limit: taskPageSize, Cursor: cursor})
		if err != nil {
			return err
		}
		for _, task := range tasks {
			visit(task)
		}
		if info == nil || !info.HasMore || strings.TrimSpace(info.NextCursor) == "" {
			return nil
		}
		cursor = info.NextCursor
	}
	return nil
}
