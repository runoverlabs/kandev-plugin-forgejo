// Package automations owns what a webhook automation condition means: the
// condition keys, each condition's settings, the provider-neutral event a
// delivery decodes into, and whether that event satisfies a condition. It forms
// no URL and knows no header; the provider package turns a delivery into an
// Event and this package decides.
package automations

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"

	"kandev-plugin-forgejo/internal/scm"
)

// Condition keys. Each must match a manifest automation_conditions entry, and
// the host requires a verified delivery's event_kind to equal it.
const (
	ConditionPush        = "push"
	ConditionPullRequest = "pull_request"
	ConditionCIResult    = "ci_result"
)

// Conditions lists every condition this plugin declares.
var Conditions = []string{ConditionPush, ConditionPullRequest, ConditionCIResult}

// Defaults is each condition's default_config. The editor copies it into a new
// trigger's settings and never consults it again, so it is the only place a
// default exists: an absent boolean is false, which is also how the editor
// renders it. manifest_test.go holds the manifest to this table.
var Defaults = map[string]map[string]any{
	ConditionPush:        {},
	ConditionPullRequest: {"on_opened": true, "exclude_draft": true},
	ConditionCIResult:    {"on_failure": true},
}

// ErrInvalidConfig wraps every settings problem. Its message is fixed text the
// host may show the operator.
var ErrInvalidConfig = errors.New("invalid condition settings")

// maxListEntries matches the host's own bound on an array setting.
const maxListEntries = 100

// Config is one condition's parsed settings. Fields a condition does not
// declare stay zero.
type Config struct {
	Key        string
	Repository string

	// push and ci_result: the branch the event is for. pull_request: the base.
	Branches []string
	Authors  []string
	Labels   []string
	// ci_result: workflow file names as Forgejo reports them (`ci.yml`).
	Workflows []string

	OnOpened, OnReopened, OnSynchronized, OnClosed, OnMerged bool
	OnReviewRequested, OnCommented, ExcludeDraft             bool

	OnSuccess, OnFailure, OnCancelled bool

	IncludeForks bool
}

// fields is the settings each condition accepts, mirroring the manifest's
// config_schema. A key outside it is refused rather than ignored, so a typo in
// a hand-written trigger cannot silently widen a filter.
var fields = map[string]map[string]string{
	ConditionPush: {"repository": "string", "branches": "array"},
	ConditionPullRequest: {
		"repository": "string", "base_branches": "array", "authors": "array", "labels": "array",
		"on_opened": "boolean", "on_reopened": "boolean", "on_synchronized": "boolean",
		"on_closed": "boolean", "on_merged": "boolean", "on_review_requested": "boolean",
		"on_commented": "boolean", "exclude_draft": "boolean", "include_forks": "boolean",
	},
	ConditionCIResult: {
		"repository": "string", "branches": "array", "workflows": "array",
		"on_success": "boolean", "on_failure": "boolean", "on_cancelled": "boolean",
		"include_forks": "boolean",
	},
}

// Fields reports the settings keys and their schema types for a condition, for
// the manifest test.
func Fields(key string) map[string]string { return fields[key] }

// Known reports whether key is a condition this plugin declares.
func Known(key string) bool {
	_, ok := fields[key]
	return ok
}

// Parse decodes and validates a condition's settings.
func Parse(key string, raw []byte) (Config, error) {
	schema, ok := fields[key]
	if !ok {
		return Config{}, fmt.Errorf("%w: unknown condition", ErrInvalidConfig)
	}
	var values map[string]any
	if err := json.Unmarshal(raw, &values); err != nil || values == nil {
		return Config{}, fmt.Errorf("%w: settings must be an object", ErrInvalidConfig)
	}
	for name, value := range values {
		kind, declared := schema[name]
		if !declared {
			return Config{}, fmt.Errorf("%w: unknown field %q", ErrInvalidConfig, name)
		}
		if !hasType(kind, value) {
			return Config{}, fmt.Errorf("%w: field %q must be a %s", ErrInvalidConfig, name, kind)
		}
	}

	cfg := Config{Key: key}
	repository, err := parseRepository(values["repository"])
	if err != nil {
		return Config{}, err
	}
	cfg.Repository = repository

	branchField := "branches"
	if key == ConditionPullRequest {
		branchField = "base_branches"
	}
	if cfg.Branches, err = parseGlobs(branchField, values[branchField]); err != nil {
		return Config{}, err
	}
	if cfg.Authors, err = parseList("authors", values["authors"]); err != nil {
		return Config{}, err
	}
	if cfg.Labels, err = parseList("labels", values["labels"]); err != nil {
		return Config{}, err
	}
	if cfg.Workflows, err = parseList("workflows", values["workflows"]); err != nil {
		return Config{}, err
	}

	flag := func(name string) bool { value, _ := values[name].(bool); return value }
	cfg.OnOpened, cfg.OnReopened = flag("on_opened"), flag("on_reopened")
	cfg.OnSynchronized, cfg.OnClosed, cfg.OnMerged = flag("on_synchronized"), flag("on_closed"), flag("on_merged")
	cfg.OnReviewRequested, cfg.OnCommented = flag("on_review_requested"), flag("on_commented")
	cfg.ExcludeDraft = flag("exclude_draft")
	cfg.OnSuccess, cfg.OnFailure, cfg.OnCancelled = flag("on_success"), flag("on_failure"), flag("on_cancelled")
	cfg.IncludeForks = flag("include_forks")
	return cfg, nil
}

func hasType(kind string, value any) bool {
	switch kind {
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "array":
		items, ok := value.([]any)
		if !ok {
			return false
		}
		for _, item := range items {
			if _, ok := item.(string); !ok {
				return false
			}
		}
		return true
	}
	return false
}

// parseRepository requires exactly `owner/name`, by the same rule as issue
// watches. The repository is the only thing tying a correctly signed delivery
// to this condition, so a missing one matches nothing rather than everything.
func parseRepository(value any) (string, error) {
	text, _ := value.(string)
	ref, err := scm.ParseRepoRef(text)
	if err != nil {
		return "", fmt.Errorf("%w: repository must be owner/name", ErrInvalidConfig)
	}
	return ref.FullName(), nil
}

func parseList(name string, value any) ([]string, error) {
	items, _ := value.([]any)
	if len(items) > maxListEntries {
		return nil, fmt.Errorf("%w: %s has more than %d entries", ErrInvalidConfig, name, maxListEntries)
	}
	var out []string
	for _, item := range items {
		if text := strings.TrimSpace(item.(string)); text != "" {
			out = append(out, text)
		}
	}
	return out, nil
}

func parseGlobs(name string, value any) ([]string, error) {
	globs, err := parseList(name, value)
	if err != nil {
		return nil, err
	}
	for _, glob := range globs {
		if _, err := path.Match(glob, ""); err != nil {
			return nil, fmt.Errorf("%w: %s has an invalid pattern %q", ErrInvalidConfig, name, glob)
		}
	}
	return globs, nil
}
