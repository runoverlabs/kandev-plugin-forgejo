package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"kandev-plugin-forgejo/internal/sourcecontrol"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// manifest mirrors only the fields these tests assert on.
type manifest struct {
	ID                  string   `yaml:"id"`
	APIVersion          int      `yaml:"api_version"`
	Version             string   `yaml:"version"`
	MinKandevVersion    string   `yaml:"min_kandev_version"`
	RepositoryProviders []string `yaml:"repository_providers"`
	Capabilities        struct {
		APIRead  []string `yaml:"api_read"`
		APIWrite []string `yaml:"api_write"`
		State    bool     `yaml:"state"`
	} `yaml:"capabilities"`
	Actions []struct {
		Key          string `yaml:"key"`
		Scope        string `yaml:"scope"`
		MaxBodyBytes int    `yaml:"max_body_bytes"`
	} `yaml:"actions"`
	AgentTools []struct {
		Name         string         `yaml:"name"`
		Description  string         `yaml:"description"`
		Surfaces     []string       `yaml:"surfaces"`
		InputSchema  map[string]any `yaml:"input_schema"`
		OutputSchema map[string]any `yaml:"output_schema"`
		Annotations  struct {
			ReadOnlyHint    *bool `yaml:"read_only_hint"`
			DestructiveHint *bool `yaml:"destructive_hint"`
			IdempotentHint  *bool `yaml:"idempotent_hint"`
			OpenWorldHint   *bool `yaml:"open_world_hint"`
		} `yaml:"annotations"`
	} `yaml:"agent_tools"`
	ReferenceSources []struct {
		Source   string `yaml:"source"`
		Provider string `yaml:"provider"`
		Kind     string `yaml:"kind"`
	} `yaml:"reference_sources"`
	Runtime struct {
		Type        string            `yaml:"type"`
		Executables map[string]string `yaml:"executables"`
	} `yaml:"runtime"`
	UI struct {
		Bundle string   `yaml:"bundle"`
		Styles []string `yaml:"styles"`
	} `yaml:"ui"`
	ConfigSchema struct {
		Required   []string `yaml:"required"`
		Properties map[string]struct {
			Type   string `yaml:"type"`
			Secret bool   `yaml:"secret"`
		} `yaml:"properties"`
	} `yaml:"config_schema"`
}

func loadManifest(t *testing.T) manifest {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "manifest.yaml"))
	require.NoError(t, err)
	var parsed manifest
	require.NoError(t, yaml.Unmarshal(raw, &parsed))
	return parsed
}

// The manifest declares the provider; the code implements it. A mismatch makes
// every native surface silently inert, so assert they agree.
func TestManifestProviderMatchesCode(t *testing.T) {
	t.Parallel()
	parsed := loadManifest(t)
	require.Equal(t, "kandev-plugin-forgejo", parsed.ID)
	require.Equal(t, []string{ProviderID}, parsed.RepositoryProviders)
	require.Len(t, parsed.ReferenceSources, 1)
	require.Equal(t, ReferenceSource, parsed.ReferenceSources[0].Source,
		"the manifest reference source and the extension's ReferenceSource must match exactly")
	require.Equal(t, ProviderID, parsed.ReferenceSources[0].Provider)
	require.Equal(t, "pull_request", parsed.ReferenceSources[0].Kind)
}

// Every action the recipe routes must be declared, or Kandev rejects the call
// before the handler runs.
func TestManifestDeclaresEveryRoutedAction(t *testing.T) {
	t.Parallel()
	parsed := loadManifest(t)
	declared := map[string]string{}
	for _, action := range parsed.Actions {
		require.NotEmpty(t, action.Scope, "action %q needs a scope", action.Key)
		require.Positive(t, action.MaxBodyBytes, "action %q needs a body limit", action.Key)
		declared[action.Key] = action.Scope
	}

	for key, wantScope := range map[string]string{
		sourcecontrol.ActionRepositoriesList:      "workspace",
		sourcecontrol.ActionRepositoriesInspect:   "workspace",
		sourcecontrol.ActionRepositoriesBranches:  "workspace",
		sourcecontrol.ActionChangeRequestsCreate:  "task",
		sourcecontrol.ActionChangeRequestsGet:     "task",
		sourcecontrol.ActionChangeRequestsDetails: "task",
		// The five writes. Task-scoped so the host hands the handler a verified
		// task, which is what the linked-pull-request rule resolves against.
		sourcecontrol.ActionChangeRequestsMerge:            "task",
		sourcecontrol.ActionChangeRequestsReview:           "task",
		sourcecontrol.ActionChangeRequestsRequestReviewers: "task",
		sourcecontrol.ActionChangeRequestsUpdateBranch:     "task",
		sourcecontrol.ActionChangeRequestsComment:          "task",
		sourcecontrol.ActionChangeRequestsLink:             "task",
		sourcecontrol.ActionChangeRequestsUnlink:           "task",
		sourcecontrol.ActionChangeRequestAssociations:      "workspace",
		ActionConnectionGet:                                "workspace",
		ActionConnectionTest:                               "workspace",
		ActionConnectionSetEnabled:                         "workspace",
		// Issue watches are workspace-scoped without exception. A task-scoped
		// watch action would hand the handler a task id in place of the
		// workspace it must filter by, which is how the native providers
		// leaked other workspaces' watch configs (#3681).
		ActionWatchesList:    "workspace",
		ActionWatchesOptions: "workspace",
		ActionWatchesCreate:  "workspace",
		ActionWatchesUpdate:  "workspace",
		ActionWatchesDelete:  "workspace",
		ActionWatchesRun:     "workspace",
		ActionWatchesReset:   "workspace",
		ActionWatchesCleanup: "workspace",
	} {
		scope, ok := declared[key]
		require.True(t, ok, "manifest does not declare routed action %q", key)
		require.Equal(t, wantScope, scope, "action %q has the wrong scope", key)
	}
	require.Len(t, parsed.Actions, 27, "an undeclared or stale action entry drifted from the routed set")
}

// The source-control contracts first shipped in v0.88.0 and agent tools in
// v0.95.0. A host below the floor ignores an unknown manifest block instead of
// refusing the install, so a lower floor here would install a plugin whose
// tools silently never appear.
func TestManifestPinsContractFloor(t *testing.T) {
	t.Parallel()
	parsed := loadManifest(t)
	require.Equal(t, "0.95.0", parsed.MinKandevVersion)
	require.Equal(t, 1, parsed.APIVersion)
	require.Equal(t, "binary", parsed.Runtime.Type)
}

// Agent tools are declared once and injected into every matching agent session
// forever after, so the manifest is the place to hold the line on their cost
// and on the invariants kandev enforces at install time.
func TestManifestAgentToolsStayWithinBudget(t *testing.T) {
	t.Parallel()
	parsed := loadManifest(t)
	require.Len(t, parsed.AgentTools, 2, "a new agent tool is a permanent context cost; weigh it deliberately")

	total := 0
	seen := map[string]struct{}{}
	for _, tool := range parsed.AgentTools {
		require.Regexp(t, `^[a-z0-9][a-z0-9_]{0,31}$`, tool.Name)
		_, duplicate := seen[tool.Name]
		require.Falsef(t, duplicate, "duplicate agent tool %q", tool.Name)
		seen[tool.Name] = struct{}{}

		require.Equalf(t, []string{"kanban-task"}, tool.Surfaces, "tool %q", tool.Name)
		require.NotEmptyf(t, tool.Description, "tool %q", tool.Name)
		require.NotEmptyf(t, tool.InputSchema, "tool %q", tool.Name)
		// kandev forces additionalProperties:false when it compiles the
		// schema, so declaring it here would only cost bytes.
		require.NotContainsf(t, tool.InputSchema, "additionalProperties", "tool %q", tool.Name)
		// An output schema is validated but also shipped to every session.
		// The results here are small and self-describing, so it is not worth
		// the context.
		require.Emptyf(t, tool.OutputSchema, "tool %q", tool.Name)
		require.Falsef(t, tool.Annotations.ReadOnlyHint != nil && tool.Annotations.DestructiveHint != nil &&
			*tool.Annotations.ReadOnlyHint && *tool.Annotations.DestructiveHint,
			"tool %q cannot be both read-only and destructive", tool.Name)

		encoded, err := json.Marshal(map[string]any{
			"name":        "kandev_kandev_plugin_forgejo_" + tool.Name,
			"description": tool.Description,
			"inputSchema": tool.InputSchema,
		})
		require.NoError(t, err)
		require.Lessf(t, len(encoded), 1024, "tool %q definition is %d bytes", tool.Name, len(encoded))
		total += len(encoded)
	}
	// Measured with kandev's own estimator (o200k_base:mcp-tool-json-v1), the
	// two tools cost 329 tokens together. This byte ceiling is the proxy this
	// repo can enforce without importing kandev-internal packages.
	require.Less(t, total, 1536, "agent_tools definitions total %d bytes", total)
}

// Least privilege: the plugin must not claim capabilities it never exercises.
//
// Each entry here is load-bearing, and the list is asserted exactly so that
// adding one is a deliberate act rather than a diff nobody reads:
//
//	tasks              attached-repository resolver, task->workspace lookup,
//	                   and the issue-watch inflight count
//	repositories       attached-repository resolver
//	workspaces         the watch poller enumerates workspaces to find due watches
//	workflows          the watch form's workflow and step pickers
//	agent_profiles     the watch form's agent picker
//	executor_profiles  the watch form's executor picker
//	api_write:tasks    a watch turning an issue into a card
//
// agent_invoke and auth remain unclaimed. auth is the highest-privilege
// capability in the manifest and nothing here needs to mint a session.
func TestManifestClaimsOnlyUsedCapabilities(t *testing.T) {
	t.Parallel()
	parsed := loadManifest(t)
	require.ElementsMatch(t, []string{
		"tasks", "repositories", "workspaces", "workflows", "agent_profiles", "executor_profiles",
	}, parsed.Capabilities.APIRead)
	require.Equal(t, []string{"tasks"}, parsed.Capabilities.APIWrite,
		"issue watches create tasks; nothing else here mutates a kandev entity")
	require.True(t, parsed.Capabilities.State, "associations and watch records are stored in Host state")
}

// The packaged binaries must exist for every platform the manifest promises.
func TestManifestExecutablesCoverAllPlatforms(t *testing.T) {
	t.Parallel()
	parsed := loadManifest(t)
	for _, platform := range []string{
		"linux-amd64", "linux-arm64", "darwin-amd64", "darwin-arm64", "windows-amd64",
	} {
		path, ok := parsed.Runtime.Executables[platform]
		require.True(t, ok, "manifest does not declare an executable for %s", platform)
		require.NotEmpty(t, path)
	}
	require.Equal(t, "server/plugin-windows-amd64.exe", parsed.Runtime.Executables["windows-amd64"],
		"the Windows executable needs its .exe suffix")
}

// The token is an operator credential and must be vaulted, not plain config.
func TestManifestMarksTokenSecret(t *testing.T) {
	t.Parallel()
	parsed := loadManifest(t)
	require.ElementsMatch(t, []string{"base_url", "api_token"}, parsed.ConfigSchema.Required)
	require.True(t, parsed.ConfigSchema.Properties["api_token"].Secret,
		"api_token must be a secret field so it is stored encrypted and masked")
	require.False(t, parsed.ConfigSchema.Properties["base_url"].Secret)
}

// The UI assets the manifest points at must actually exist in the repo, or the
// packaged plugin registers nothing in the browser.
func TestManifestUIAssetsExist(t *testing.T) {
	t.Parallel()
	parsed := loadManifest(t)
	require.Equal(t, "/ui/bundle.js", parsed.UI.Bundle)
	for _, asset := range append([]string{parsed.UI.Bundle}, parsed.UI.Styles...) {
		_, err := os.Stat(filepath.Join("..", "..", filepath.FromSlash(asset)))
		require.NoError(t, err, "manifest references missing UI asset %q", asset)
	}
}
