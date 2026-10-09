//go:build !kandev_min

package watches

import (
	"context"
	"errors"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

// archivingHost is a fakeHost that also speaks the exact v2 contract, with a
// scripted archive outcome.
type archivingHost struct {
	*fakeHost
	granted  bool
	outcomes []pluginsdk.CommandStatus
	archived []pluginsdk.ExactTaskArchive
}

func (h *archivingHost) GetCapabilityContext(context.Context, string) (*pluginsdk.CapabilityContext, error) {
	return &pluginsdk.CapabilityContext{
		ApprovalRevision: 3, ManifestDigest: "digest",
		Operations: []pluginsdk.CapabilityOperation{{Method: archiveOperation, Supported: true, Authorized: h.granted}},
	}, nil
}

func (h *archivingHost) UpdateTaskExact(context.Context, pluginsdk.ExactTaskUpdate) (*pluginsdk.CommandResult, *pluginsdk.Task, error) {
	return nil, nil, errors.New("unused")
}

func (h *archivingHost) ManagedAgentConversations() pluginsdk.ManagedAgentConversationManager {
	return nil
}

func (h *archivingHost) TaskCommands() pluginsdk.ExactTaskCommandManager {
	return archiveCommands{host: h}
}

type archiveCommands struct {
	pluginsdk.ExactTaskCommandManager
	host *archivingHost
}

func (c archiveCommands) Archive(_ context.Context, in pluginsdk.ExactTaskArchive) (*pluginsdk.CommandResult, *pluginsdk.Task, error) {
	c.host.archived = append(c.host.archived, in)
	status := pluginsdk.CommandApplied
	if len(c.host.outcomes) > 0 {
		status, c.host.outcomes = c.host.outcomes[0], c.host.outcomes[1:]
	}
	if status == pluginsdk.CommandApplied {
		stamp := "2026-09-19T12:00:00Z"
		c.host.fakeHost.mu.Lock()
		for i := range c.host.tasks {
			if c.host.tasks[i].ID == in.TaskID {
				c.host.tasks[i].ArchivedAt = &stamp
			}
		}
		c.host.fakeHost.mu.Unlock()
	}
	if status == pluginsdk.CommandConflict {
		// A conflict means the task moved on since it was listed.
		c.host.fakeHost.mu.Lock()
		for i := range c.host.tasks {
			if c.host.tasks[i].ID == in.TaskID {
				c.host.tasks[i].ResourceVersion = in.ExpectedResourceVersion + "+1"
			}
		}
		c.host.fakeHost.mu.Unlock()
	}
	return &pluginsdk.CommandResult{Status: status}, nil, nil
}
