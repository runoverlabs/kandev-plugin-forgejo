//go:build !kandev_min

package watches

import (
	"context"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

// The exact archive command exists in the SDK from Kandev 0.97.0. The minimum
// supported SDK predates it and builds archive_min.go instead (tag kandev_min),
// where cleanup can only complete a task. A binary built here still runs against
// an older host: its Host does not implement the extension, and archive falls
// back at run time.

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
