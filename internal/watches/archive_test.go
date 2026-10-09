//go:build !kandev_min

package watches

import (
	"context"
	"testing"

	"github.com/kandev/kandev/pkg/pluginsdk"
	"github.com/stretchr/testify/require"
)

func archivingRig(t *testing.T, granted bool, outcomes ...pluginsdk.CommandStatus) (*reviewRig, *archivingHost) {
	t.Helper()
	base := newFakeHost("ws-1")
	host := &archivingHost{fakeHost: base, granted: granted, outcomes: outcomes}
	rig := rigOver(t, base, host)
	rig.watch.CleanupPolicy = CleanupWhenClosed
	return rig, host
}

func TestCleanupArchivesWhenTheOperatorGrantedIt(t *testing.T) {
	t.Parallel()
	rig, host := archivingRig(t, true)
	taskID := filedPR(t, rig, 7)
	rig.host.tasks[0].ResourceVersion = "2026-09-19T11:00:00Z"
	rig.finish(7, PullClosed)

	result := rig.run(t)
	require.Equal(t, 1, result.Archived)
	require.Zero(t, result.Completed)
	require.Empty(t, result.CleanupNote)
	require.Len(t, host.archived, 1)
	require.Equal(t, taskID, host.archived[0].TaskID)
	require.Equal(t, "2026-09-19T11:00:00Z", host.archived[0].ExpectedResourceVersion)
	require.Equal(t, uint64(3), host.archived[0].ApprovalRevision)
	require.Empty(t, host.updated, "an archived task is not also completed")
	// Archived tasks cost nothing on the next pass.
	rig.reviews.checked = nil
	rig.run(t)
	require.Empty(t, rig.reviews.checked)
}

func TestCleanupFallsBackToCompletingWhenArchiveIsDenied(t *testing.T) {
	t.Parallel()
	for _, status := range []pluginsdk.CommandStatus{pluginsdk.CommandDenied, pluginsdk.CommandUnsupported} {
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()
			rig, _ := archivingRig(t, true, status)
			filedPR(t, rig, 7)
			rig.finish(7, PullMerged)
			result := rig.run(t)
			require.Equal(t, 1, result.Completed)
			require.NotEmpty(t, result.CleanupNote)
		})
	}
}

func TestCleanupRetriesArchiveOnceOnAVersionConflict(t *testing.T) {
	t.Parallel()
	rig, host := archivingRig(t, true, pluginsdk.CommandConflict, pluginsdk.CommandApplied)
	filedPR(t, rig, 7)
	rig.host.tasks[0].ResourceVersion = "2026-09-19T11:30:00Z"
	rig.finish(7, PullMerged)
	require.Equal(t, 1, rig.run(t).Archived)
	require.Len(t, host.archived, 2)
	require.Equal(t, "2026-09-19T11:30:00Z+1", host.archived[1].ExpectedResourceVersion)
	require.NotEqual(t, host.archived[0].IdempotencyKey, host.archived[1].IdempotencyKey, "a retry at a new version is a new command")
}

func TestCleanupGivesUpAfterTwoConflictsAndCompletes(t *testing.T) {
	t.Parallel()
	rig, host := archivingRig(t, true, pluginsdk.CommandConflict, pluginsdk.CommandConflict)
	filedPR(t, rig, 7)
	rig.finish(7, PullMerged)
	result := rig.run(t)
	require.Len(t, host.archived, 2)
	require.Equal(t, 1, result.Completed)
}

func TestCleanupNoteClearsOnceArchivingWorks(t *testing.T) {
	t.Parallel()
	rig, host := archivingRig(t, false)
	filedPR(t, rig, 7)
	filedPR(t, rig, 8)
	rig.finish(7, PullMerged)
	rig.run(t)
	stored, _ := rig.store.Get(context.Background(), "ws-1", rig.watch.ID)
	require.Equal(t, ArchiveGrantNote, stored.LastCleanupNote)

	host.granted = true
	rig.watch = stored
	rig.finish(8, PullMerged)
	require.Equal(t, 1, rig.run(t).Archived)
	stored, _ = rig.store.Get(context.Background(), "ws-1", rig.watch.ID)
	require.Empty(t, stored.LastCleanupNote)
}
