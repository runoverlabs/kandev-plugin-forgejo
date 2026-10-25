package forgejo

import (
	"context"
	"errors"
	"testing"

	"kandev-plugin-forgejo/internal/scm"
	"kandev-plugin-forgejo/internal/sourcecontrol"

	"github.com/kandev/kandev/pkg/pluginsdk"
	"github.com/stretchr/testify/require"
)

func identity(scope, repositoryID string, number int64) sourcecontrol.ChangeRequestIdentity {
	return sourcecontrol.ChangeRequestIdentity{ConnectionScope: scope, RepositoryID: repositoryID, Number: number}
}

func hostWithTask(taskID, workspaceID string) *fakeHost {
	host := newFakeHost(map[string]any{})
	host.tasks[taskID] = &pluginsdk.Task{ID: taskID, WorkspaceID: workspaceID}
	return host
}

func TestLinkAndUnlinkRoundTrip(t *testing.T) {
	t.Parallel()
	host := hostWithTask("task-1", "workspace-1")
	associations := NewAssociations(func() pluginsdk.Host { return host })
	key := identity("https://forge.example.com", "7", 42)

	require.NoError(t, associations.Link(context.Background(), "task-1", key))

	stored, taskIDs, err := associations.ListForWorkspace(context.Background(), "workspace-1")
	require.NoError(t, err)
	require.Len(t, stored, 1)
	require.Equal(t, key, stored[0], "number must survive the JSON float64 round trip")
	require.Equal(t, []string{"task-1"}, taskIDs)

	require.NoError(t, associations.Unlink(context.Background(), "task-1", key))
	stored, _, err = associations.ListForWorkspace(context.Background(), "workspace-1")
	require.NoError(t, err)
	require.Empty(t, stored)
}

// Linking twice must not create a second entry, and unlinking a missing link
// is not an error — event delivery and UI retries are both at-least-once.
func TestLinkIsIdempotentAndUnlinkToleratesMissing(t *testing.T) {
	t.Parallel()
	host := hostWithTask("task-1", "workspace-1")
	associations := NewAssociations(func() pluginsdk.Host { return host })
	key := identity("https://forge.example.com", "7", 42)

	require.NoError(t, associations.Link(context.Background(), "task-1", key))
	require.NoError(t, associations.Link(context.Background(), "task-1", key))
	stored, _, err := associations.ListForWorkspace(context.Background(), "workspace-1")
	require.NoError(t, err)
	require.Len(t, stored, 1)

	require.NoError(t, associations.Unlink(context.Background(), "task-1", identity("https://forge.example.com", "7", 99)))
}

// Distinct pull requests on one task each get their own key, so two concurrent
// links cannot clobber one another through a shared read-modify-write value.
func TestSeparateLinksUseSeparateKeys(t *testing.T) {
	t.Parallel()
	host := hostWithTask("task-1", "workspace-1")
	associations := NewAssociations(func() pluginsdk.Host { return host })

	require.NoError(t, associations.Link(context.Background(), "task-1", identity("https://forge.example.com", "7", 1)))
	require.NoError(t, associations.Link(context.Background(), "task-1", identity("https://forge.example.com", "8", 1)))
	require.NoError(t, associations.Link(context.Background(), "task-1", identity("https://forge.example.com", "7", 2)))

	stored, _, err := associations.ListForWorkspace(context.Background(), "workspace-1")
	require.NoError(t, err)
	require.Len(t, stored, 3)
}

func TestListForTaskFiltersByTask(t *testing.T) {
	t.Parallel()
	host := hostWithTask("task-1", "workspace-1")
	host.tasks["task-2"] = &pluginsdk.Task{ID: "task-2", WorkspaceID: "workspace-1"}
	associations := NewAssociations(func() pluginsdk.Host { return host })

	require.NoError(t, associations.Link(context.Background(), "task-1", identity("s", "7", 1)))
	require.NoError(t, associations.Link(context.Background(), "task-2", identity("s", "7", 2)))

	forTask, err := associations.ListForTask(context.Background(), "workspace-1", "task-1")
	require.NoError(t, err)
	require.Len(t, forTask, 1)
	require.Equal(t, int64(1), forTask[0].Number)
}

func TestCorruptEntriesAreSkippedNotFatal(t *testing.T) {
	t.Parallel()
	host := hostWithTask("task-1", "workspace-1")
	associations := NewAssociations(func() pluginsdk.Host { return host })
	require.NoError(t, associations.Link(context.Background(), "task-1", identity("s", "7", 1)))
	// A half-written entry and an unrelated key must not break the refresh.
	require.NoError(t, host.SetState(context.Background(), "workspace", "workspace-1", "task:broken", map[string]any{"number": 0}))
	require.NoError(t, host.SetState(context.Background(), "workspace", "workspace-1", "settings", map[string]any{"x": 1}))

	stored, _, err := associations.ListForWorkspace(context.Background(), "workspace-1")
	require.NoError(t, err)
	require.Len(t, stored, 1)
}

func TestAssociationErrorsSurface(t *testing.T) {
	t.Parallel()
	host := hostWithTask("task-1", "workspace-1")
	associations := NewAssociations(func() pluginsdk.Host { return host })

	host.stateErr = errors.New("state unavailable")
	require.ErrorContains(t, associations.Link(context.Background(), "task-1", identity("s", "7", 1)), "store task association")

	host.stateErr = nil
	host.taskErr = errors.New("task unavailable")
	require.ErrorContains(t, associations.Link(context.Background(), "task-1", identity("s", "7", 1)), "resolve task workspace")

	host.taskErr = nil
	require.ErrorContains(t, associations.Link(context.Background(), "unknown-task", identity("s", "7", 1)), "has no workspace")
	require.ErrorContains(t, associations.Link(context.Background(), "  ", identity("s", "7", 1)), "task id is required")

	missingHost := NewAssociations(func() pluginsdk.Host { return nil })
	require.ErrorIs(t, missingHost.Link(context.Background(), "task-1", identity("s", "7", 1)), scm.ErrNotConfigured)
}

func TestAssociationKeyStaysWithinHostKeyVocabulary(t *testing.T) {
	t.Parallel()
	key := associationKey("task/with:odd chars", identity("s", "repo/9", 3))
	require.Equal(t, "task:task_with_odd_chars:repo_9:3", key)
	require.Regexp(t, `^[a-zA-Z0-9][a-zA-Z0-9._:-]*$`, key)
}

func TestDecodeNumberAcceptsHostEncodings(t *testing.T) {
	t.Parallel()
	for _, value := range []any{float64(42), int64(42), 42, "42"} {
		number, ok := decodeNumber(value)
		require.True(t, ok)
		require.Equal(t, int64(42), number)
	}
	for _, value := range []any{float64(1.5), 0, "", "abc", nil} {
		_, ok := decodeNumber(value)
		require.False(t, ok)
	}
}
