package forgejo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"kandev-plugin-forgejo/internal/forgejo/rest"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

// errCredentialScope is returned for any request outside what this plugin
// will authorize. It never carries request or credential values.
var errCredentialScope = errors.New("forgejo: git credential request is outside the configured instance's repository scope")

// GitCredentials backs Kandev's provider-neutral Git credential broker. Kandev
// clones plugin-provider repositories over HTTPS and asks the owning plugin for
// transient credentials; without them a task on a Forgejo/Gitea repository
// cannot start. The configured access token is the credential. It is returned
// only for repository paths on the configured instance and is never logged.
type GitCredentials struct {
	connection *Connection
}

// NewGitCredentials returns the credential adapter for connection.
func NewGitCredentials(connection *Connection) *GitCredentials {
	return &GitCredentials{connection: connection}
}

// Resolve returns the token owner's login and the token for a complete,
// in-scope lease request.
func (g *GitCredentials) Resolve(ctx context.Context, request *pluginsdk.ResolveGitCredentialRequest) (*pluginsdk.ResolveGitCredentialResponse, error) {
	if request == nil {
		return nil, errCredentialScope
	}
	client, err := g.scopedClient(ctx, credentialLeaseScope{
		workspaceID: request.WorkspaceID, taskID: request.TaskID, sessionID: request.SessionID,
		repositoryID: request.RepositoryID, host: request.Host, path: request.Path,
	})
	if err != nil {
		return nil, err
	}
	user, err := client.CurrentUser(ctx)
	if err != nil {
		return nil, fmt.Errorf("forgejo: resolve token owner for git credential: %w", err)
	}
	if strings.TrimSpace(user.Login) == "" {
		return nil, errors.New("forgejo: token owner has no login")
	}
	return &pluginsdk.ResolveGitCredentialResponse{Username: user.Login, Secret: client.Token()}, nil
}

// Binding returns a non-secret revision for the exact lease scope. It changes
// whenever the instance URL or token changes, so Kandev revokes helper leases
// issued under a rotated or removed credential. It needs no instance I/O.
func (g *GitCredentials) Binding(ctx context.Context, request *pluginsdk.GitCredentialBindingRequest) (*pluginsdk.GitCredentialBindingResponse, error) {
	if request == nil {
		return nil, errCredentialScope
	}
	scope := credentialLeaseScope{
		workspaceID: request.WorkspaceID, taskID: request.TaskID, sessionID: request.SessionID,
		repositoryID: request.RepositoryID, host: request.Host, path: request.Path,
	}
	client, err := g.scopedClient(ctx, scope)
	if err != nil {
		return nil, err
	}
	digest := sha256.New()
	for _, part := range []string{
		client.Scope(), client.Token(), scope.workspaceID, scope.taskID, scope.sessionID,
		scope.repositoryID, strings.ToLower(scope.host), scope.path,
	} {
		digest.Write([]byte(part))
		digest.Write([]byte{0})
	}
	return &pluginsdk.GitCredentialBindingResponse{Binding: "v1:" + hex.EncodeToString(digest.Sum(nil))}, nil
}

type credentialLeaseScope struct {
	workspaceID, taskID, sessionID, repositoryID, host, path string
}

// scopedClient checks the request before any instance I/O: complete identity,
// the configured instance host, and a single owner/repository path below the
// instance base path.
func (g *GitCredentials) scopedClient(ctx context.Context, scope credentialLeaseScope) (*rest.Client, error) {
	for _, value := range []string{scope.workspaceID, scope.taskID, scope.sessionID, scope.repositoryID} {
		if strings.TrimSpace(value) == "" {
			return nil, errCredentialScope
		}
	}
	client, err := g.connection.Client(ctx)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(strings.TrimSpace(scope.host), client.Host()) {
		return nil, errCredentialScope
	}
	if !isRepositoryPath(scope.path, client.BasePath()) {
		return nil, errCredentialScope
	}
	return client, nil
}

// isRepositoryPath reports whether path names exactly one owner/repository
// (optionally ending in .git) directly below basePath.
func isRepositoryPath(path, basePath string) bool {
	prefix := strings.TrimSuffix(basePath, "/") + "/"
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	segments := strings.Split(strings.TrimSuffix(strings.TrimPrefix(path, prefix), ".git"), "/")
	if len(segments) != 2 {
		return false
	}
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}
