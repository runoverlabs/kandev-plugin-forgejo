// Package prautomation is the provider-neutral half of pull request
// automation: CI auto-fix, auto-merge and lifecycle prompts for the pull
// requests a task has linked. It owns every decision — when to prompt an
// agent, when to merge, what counts as feedback — and reaches the instance,
// the agent and Kandev's state only through ports. Nothing here knows a
// Forgejo URL.
package prautomation

import (
	"errors"
	"strings"
	"unicode/utf8"
)

// Switches are the per pull request automation choices. Each is off unless a
// human turned it on, directly or through the workspace defaults.
type Switches struct {
	AutoFix           bool `json:"auto_fix"`
	AutoMerge         bool `json:"auto_merge"`
	OnReviewRequested bool `json:"on_review_requested"`
	OnMerged          bool `json:"on_merged"`
	OnClosed          bool `json:"on_closed"`
}

// Any reports whether anything is switched on, which is what makes a pull
// request worth observing.
func (s Switches) Any() bool {
	return s.AutoFix || s.AutoMerge || s.OnReviewRequested || s.OnMerged || s.OnClosed
}

// lifecycle reports whether any lifecycle prompt is on.
func (s Switches) lifecycle() bool {
	return s.OnReviewRequested || s.OnMerged || s.OnClosed
}

// Trust decides whose review text may reach the agent.
type Trust string

const (
	// TrustEveryone counts every review and comment, as Kandev's GitHub
	// integration does. Anyone who can comment puts text in front of the agent.
	TrustEveryone Trust = "everyone"
	// TrustOfficial counts official reviews (the reviewer's approval counts
	// towards the branch's required approvals, which on every supported host
	// means write access) with their inline comments, and conversation
	// comments by someone who left an official review on the pull request.
	//
	// A non-admin token cannot read another user's permission level (the
	// permission endpoint answers 403), so the review's own flag is the only
	// trust signal every operator token can see.
	TrustOfficial Trust = "official"
	// TrustNone sends no review text at all: CI failures and conflicts only.
	TrustNone Trust = "none"
)

// Valid reports whether t is one of the known levels.
func (t Trust) Valid() bool {
	return t == TrustEveryone || t == TrustOfficial || t == TrustNone
}

// MaxTemplateBytes bounds the workspace's auto-fix template.
const MaxTemplateBytes = 16 << 10

// Settings are the workspace's automation settings.
type Settings struct {
	// Defaults are copied into a pull request's switches the first time the
	// plugin sees it linked. Changing them later does not touch pull requests
	// already seen.
	Defaults Switches `json:"defaults"`
	Trust    Trust    `json:"trust"`
	// Template is the auto-fix prompt. Empty means DefaultTemplate.
	Template string `json:"template"`
}

// Normalize fills the defaults a stored or partial value leaves out.
func (s Settings) Normalize() Settings {
	if !s.Trust.Valid() {
		s.Trust = TrustEveryone
	}
	if strings.TrimSpace(s.Template) == "" {
		s.Template = ""
	}
	return s
}

// Validate checks settings coming from the panel.
func (s Settings) Validate() error {
	if !s.Trust.Valid() {
		return errors.New("trust must be everyone, official or none")
	}
	if len(s.Template) > MaxTemplateBytes {
		return errors.New("the auto-fix prompt is limited to 16 KiB")
	}
	if !utf8.ValidString(s.Template) {
		return errors.New("the auto-fix prompt is not valid text")
	}
	return nil
}

// EffectiveTemplate is the template the next prompt is rendered from.
func (s Settings) EffectiveTemplate() string {
	if strings.TrimSpace(s.Template) == "" {
		return DefaultTemplate
	}
	return s.Template
}
