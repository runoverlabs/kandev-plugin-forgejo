package watches

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

// maxCleanupChecks bounds the provider requests one cleanup pass makes. Each
// live task costs one, and a watch that has filed a long backlog must not turn
// one tick into hundreds of requests; the rest wait for the next tick.
const maxCleanupChecks = 40

// archiveOperation is the capability context's name for the exact archive
// command, and the one this pass needs the operator to have granted.
const archiveOperation = "ArchiveTaskExact"

// ArchiveGrantNote is shown when tasks are completed because archiving is not
// granted. The operator fixes it in the host's approvals, not here.
const ArchiveGrantNote = "Tasks are completed, not archived: the host has not approved this plugin archiving tasks."

const archiveFailedNote = "The host could not archive a task, so it was completed instead."

const archiveUnsupportedNote = "This host cannot archive tasks from a plugin, so tasks are completed instead."

// taskDone reports whether a task has already left the board, by archive or by
// completion. Such a task costs no provider request.
//
// The state is checked as well as the timestamps because setting a task's state
// to COMPLETED, which is how a plugin ends a task it may not archive, does not
// set its completed_at (checked on Kandev 0.97.0). A failed task is not done: its
// pull request may still finish and deserve a cleanup.
func taskDone(task pluginsdk.Task) bool {
	if task.ArchivedAt != nil || task.CompletedAt != nil {
		return true
	}
	switch strings.ToUpper(task.State) {
	case "COMPLETED", "CANCELLED":
		return true
	}
	return false
}

// cleanup retires the tasks of pull requests that are no longer open, per the
// watch's policy. It never deletes: a plugin cannot, and the card is the
// record of a review that happened.
func (p *Poller) cleanup(ctx context.Context, host pluginsdk.Host, watch Watch, result *Result) []string {
	if watch.CleanupPolicy != CleanupWhenClosed || p.reviews == nil {
		// never: zero provider requests and zero host reads.
		return nil
	}
	records, err := p.store.Records(ctx, watch)
	if err != nil {
		return []string{"cleanup: " + err.Error()}
	}
	if len(records) == 0 {
		return nil
	}
	tasks, err := p.allTasks(ctx, host, watch.WorkspaceID)
	if err != nil {
		return []string{"cleanup: " + err.Error()}
	}

	var failures []string
	checks := 0
	for _, record := range records {
		if ctx.Err() != nil || checks >= maxCleanupChecks {
			break
		}
		task, exists := tasks[record.TaskID]
		if exists && taskDone(task) {
			continue
		}
		repo, err := ParseRepoRef(record.Repo)
		if err != nil {
			continue
		}
		checks++
		state, err := p.reviews.PullState(ctx, repo, record.Number)
		if err != nil {
			failures = append(failures, fmt.Sprintf("cleanup %s#%d: %s", record.Repo, record.Number, err))
			if errors.Is(err, ErrBackoff) {
				break
			}
			continue
		}
		if state == PullOpen {
			continue
		}
		if !exists {
			// The task is gone and the pull request is finished, so the record
			// has no further use. Dropped only now: while the pull request is
			// open the record is what stops the same request being filed again.
			if err := host.DeleteState(ctx, stateScope, watch.WorkspaceID, record.Key); err != nil {
				failures = append(failures, fmt.Sprintf("cleanup %s#%d: %s", record.Repo, record.Number, err))
			}
			continue
		}
		archived, why, err := p.retire(ctx, host, task)
		if err != nil {
			failures = append(failures, fmt.Sprintf("cleanup %s#%d: %s", record.Repo, record.Number, err))
			continue
		}
		if archived {
			result.Archived++
		} else {
			result.Completed++
			result.CleanupNote = why
		}
	}
	return failures
}

// allTasks reads the workspace's tasks, archived ones included, in one walk.
func (p *Poller) allTasks(ctx context.Context, host pluginsdk.Host, workspaceID string) (map[string]pluginsdk.Task, error) {
	found := map[string]pluginsdk.Task{}
	cursor := ""
	for page := 0; page < maxTaskPages; page++ {
		tasks, info, err := host.Tasks().List(ctx, pluginsdk.TaskFilter{
			WorkspaceIDs: []string{workspaceID}, IncludeArchived: true,
		}, pluginsdk.Page{Limit: taskPageSize, Cursor: cursor})
		if err != nil {
			return nil, fmt.Errorf("list tasks: %w", err)
		}
		for _, task := range tasks {
			found[task.ID] = task
		}
		if info == nil || !info.HasMore || strings.TrimSpace(info.NextCursor) == "" {
			break
		}
		cursor = info.NextCursor
	}
	return found, nil
}

// retire archives a task when the host allows it and completes it when not.
// When it completes, the second return says why archiving was not used.
func (p *Poller) retire(ctx context.Context, host pluginsdk.Host, task pluginsdk.Task) (archived bool, why string, err error) {
	why = archive(ctx, host, task)
	if why == "" {
		return true, "", nil
	}
	state := "COMPLETED"
	if _, err := host.Tasks().Update(ctx, pluginsdk.UpdateTaskInput{ID: task.ID, State: &state}); err != nil {
		return false, "", fmt.Errorf("complete task: %w", err)
	}
	return false, why, nil
}

// archive archives the task through the host's exact command and returns "" on
// success, or the reason it could not.
func archive(ctx context.Context, host pluginsdk.Host, task pluginsdk.Task) string {
	exact, ok := pluginsdk.HostV2(host)
	if !ok {
		return archiveUnsupportedNote
	}
	commands, ok := pluginsdk.HostTaskCommands(host)
	if !ok {
		return archiveUnsupportedNote
	}
	granted, capability := archiveGranted(ctx, exact, task.WorkspaceID)
	if !granted {
		return ArchiveGrantNote
	}
	version := task.ResourceVersion
	for attempt := 0; attempt < 2; attempt++ {
		outcome, _, err := commands.Archive(ctx, pluginsdk.ExactTaskArchive{
			RequestID:               "archive-" + task.ID,
			WorkspaceID:             task.WorkspaceID,
			TaskID:                  task.ID,
			IdempotencyKey:          "forgejo-archive-" + task.ID + "-" + version,
			ExpectedResourceVersion: version,
			ApprovalRevision:        capability.ApprovalRevision,
			ManifestDigest:          capability.ManifestDigest,
		})
		if err != nil || outcome == nil {
			return archiveFailedNote
		}
		switch outcome.Status {
		case pluginsdk.CommandApplied, pluginsdk.CommandAlreadyApplied, pluginsdk.CommandNoChange:
			return ""
		case pluginsdk.CommandConflict:
			// The task changed since it was listed. Re-read it and try once more.
			fresh, err := host.Tasks().Get(ctx, task.ID)
			if err != nil || fresh == nil {
				return archiveFailedNote
			}
			if taskDone(*fresh) {
				return ""
			}
			version = fresh.ResourceVersion
		case pluginsdk.CommandDenied, pluginsdk.CommandUnsupported:
			return ArchiveGrantNote
		default:
			return archiveFailedNote
		}
	}
	return archiveFailedNote
}

// ArchiveGranted reports whether the operator has granted archiving for a
// workspace. The panel asks it too, so an unusable setting is visible before a
// tick finds out.
func ArchiveGranted(ctx context.Context, host pluginsdk.Host, workspaceID string) bool {
	exact, ok := pluginsdk.HostV2(host)
	if !ok {
		return false
	}
	granted, _ := archiveGranted(ctx, exact, workspaceID)
	return granted
}

func archiveGranted(ctx context.Context, exact pluginsdk.ExactHost, workspaceID string) (bool, *pluginsdk.CapabilityContext) {
	capability, err := exact.GetCapabilityContext(ctx, workspaceID)
	if err != nil || capability == nil || capability.ApprovalRevision == 0 {
		return false, nil
	}
	for _, operation := range capability.Operations {
		if operation.Method == archiveOperation {
			return operation.Supported && operation.Authorized, capability
		}
	}
	return false, nil
}

// ErrCleanupOff means a manual cleanup was asked of a watch whose policy is to
// leave tasks alone.
var ErrCleanupOff = errors.New("watches: cleanup is off for this watch")

// Cleanup runs the cleanup pass for one review watch on demand, without a
// discovery poll and without touching the poll clock.
func (p *Poller) Cleanup(ctx context.Context, watch Watch) (Result, error) {
	result := Result{WatchID: watch.ID, Budget: watch.MaxInflightTasks}
	if !watch.IsReview() || watch.CleanupPolicy != CleanupWhenClosed {
		return result, ErrCleanupOff
	}
	host, err := p.store.host()
	if err != nil {
		return result, err
	}
	result.Errors = p.cleanup(ctx, host, watch, &result)
	switch {
	case result.CleanupNote != "":
		watch.LastCleanupNote = result.CleanupNote
	case result.Archived > 0:
		watch.LastCleanupNote = ""
	}
	if watch.LastCleanupNote != "" || result.Archived > 0 {
		if _, err := p.store.Put(ctx, watch); err != nil {
			return result, err
		}
	}
	return result, nil
}
