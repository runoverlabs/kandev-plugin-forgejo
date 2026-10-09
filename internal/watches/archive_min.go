//go:build kandev_min

package watches

import (
	"context"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

// Built against the minimum supported SDK, which has no exact archive command.
// Cleanup completes tasks instead.

func archive(context.Context, pluginsdk.Host, pluginsdk.Task) string { return archiveUnsupportedNote }

// ArchiveGranted is always false against the minimum SDK.
func ArchiveGranted(context.Context, pluginsdk.Host, string) bool { return false }
