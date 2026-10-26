// Package kandev is how every layer reaches Kandev: the late-bound Host, the
// plugin config, the workspaces it serves, and the JSON responses an action
// returns. It knows nothing about Forgejo.
package kandev

import (
	"context"
	"errors"
	"strings"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

// ErrNoHost means the plugin has not finished connecting back to Kandev. It is
// transient: the broker dial happens after the plugin starts serving.
var ErrNoHost = errors.New("kandev-plugin-forgejo: kandev host is not available yet")

// Provider defers Host resolution to call time. The Host is injected from a
// background goroutine after construction, so nothing may capture it earlier.
type Provider func() pluginsdk.Host

// Get returns the Host, or ErrNoHost while it has not arrived.
func (p Provider) Get() (pluginsdk.Host, error) {
	if p == nil {
		return nil, ErrNoHost
	}
	if host := p(); host != nil {
		return host, nil
	}
	return nil, ErrNoHost
}

// Bounds on the workspace walk: the Host caps a page at 200, and the page
// count guards against a cursor that never ends.
const (
	workspacePageSize = 200
	maxWorkspacePages = 50
)

// WorkspaceIDs enumerates every workspace the host serves, page by page.
func WorkspaceIDs(ctx context.Context, host pluginsdk.Host) ([]string, error) {
	var ids []string
	cursor := ""
	for range maxWorkspacePages {
		workspaces, pageInfo, err := host.Workspaces().List(ctx, pluginsdk.Page{Limit: workspacePageSize, Cursor: cursor})
		if err != nil {
			return nil, err
		}
		for _, workspace := range workspaces {
			if id := strings.TrimSpace(workspace.ID); id != "" {
				ids = append(ids, id)
			}
		}
		if pageInfo == nil || !pageInfo.HasMore || pageInfo.NextCursor == "" || pageInfo.NextCursor == cursor {
			break
		}
		cursor = pageInfo.NextCursor
	}
	return ids, nil
}
