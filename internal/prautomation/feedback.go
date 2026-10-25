package prautomation

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// Feedback kinds.
const (
	KindCheck    = "check"
	KindConflict = "conflict"
	KindReview   = "review"
	KindInline   = "inline"
	KindComment  = "comment"
)

// Item is one piece of feedback an auto-fix prompt can carry. Key identifies it
// across observations: the checkpoint is the set of keys already handled, so a
// key must change when the feedback is new and stay put when it is not.
type Item struct {
	Key    string `json:"key"`
	Kind   string `json:"kind"`
	Author string `json:"author,omitempty"`
	Bot    bool   `json:"bot,omitempty"`
	Name   string `json:"name,omitempty"`
	State  string `json:"state,omitempty"`
	Path   string `json:"path,omitempty"`
	Body   string `json:"body,omitempty"`
	URL    string `json:"url,omitempty"`
}

// Snapshot is the feedback present on a pull request after the trust filter.
type Snapshot struct {
	Items []Item
	// Ignored counts review text left out by the trust level, so a narrow
	// setting is visible in the panel rather than silent.
	Ignored int
}

// BuildSnapshot collects the feedback on obs that trust lets through.
//
// A failing check and a conflict are keyed to the head commit: the same check
// failing again after the agent pushed is new feedback, as it is on GitHub,
// where every run has its own id. Reviews and comments are keyed by their id.
// The operator's own words are never feedback — the agent posts as that
// account, and counting them would loop it on its own replies.
func BuildSnapshot(obs Observation, trust Trust) Snapshot {
	var snapshot Snapshot
	for _, check := range obs.CI.Failing {
		snapshot.Items = append(snapshot.Items, Item{
			Key:   fmt.Sprintf("check:%s@%s", check.Name, obs.HeadSHA),
			Kind:  KindCheck,
			Name:  check.Name,
			State: check.State,
			URL:   check.URL,
		})
	}
	if obs.Mergeable != nil && !*obs.Mergeable {
		snapshot.Items = append(snapshot.Items, Item{
			Key:  "conflict@" + obs.HeadSHA,
			Kind: KindConflict,
		})
	}

	operator := strings.ToLower(strings.TrimSpace(obs.Operator))
	own := func(author string) bool {
		return operator != "" && strings.ToLower(strings.TrimSpace(author)) == operator
	}
	officialAuthors := map[string]bool{}
	for _, review := range obs.Reviews {
		if review.Official && !review.Dismissed {
			officialAuthors[strings.ToLower(review.Author)] = true
		}
	}
	admit := func(official bool) bool {
		switch trust {
		case TrustNone:
			return false
		case TrustOfficial:
			return official
		default:
			return true
		}
	}

	for _, review := range obs.Reviews {
		if review.Dismissed || own(review.Author) {
			continue
		}
		if review.State == ReviewChangesRequested ||
			(review.State == ReviewCommented && strings.TrimSpace(review.Body) != "") {
			if admit(review.Official) {
				snapshot.Items = append(snapshot.Items, Item{
					Key:    fmt.Sprintf("review:%d", review.ID),
					Kind:   KindReview,
					Author: review.Author,
					Bot:    review.Bot,
					State:  review.State,
					Body:   review.Body,
				})
			} else {
				snapshot.Ignored++
			}
		}
		for _, comment := range review.Comments {
			if comment.Resolved || own(comment.Author) || strings.TrimSpace(comment.Body) == "" {
				continue
			}
			if !admit(review.Official) {
				snapshot.Ignored++
				continue
			}
			snapshot.Items = append(snapshot.Items, Item{
				Key:    fmt.Sprintf("inline:%d", comment.ID),
				Kind:   KindInline,
				Author: comment.Author,
				Bot:    comment.Bot,
				Path:   comment.Path,
				Body:   comment.Body,
			})
		}
	}
	for _, comment := range obs.Comments {
		if own(comment.Author) || strings.TrimSpace(comment.Body) == "" {
			continue
		}
		if !admit(officialAuthors[strings.ToLower(comment.Author)]) {
			snapshot.Ignored++
			continue
		}
		snapshot.Items = append(snapshot.Items, Item{
			Key:    fmt.Sprintf("comment:%d", comment.ID),
			Kind:   KindComment,
			Author: comment.Author,
			Bot:    comment.Bot,
			Body:   comment.Body,
		})
	}
	return snapshot
}

// Delta returns the items whose keys are not in the checkpoint.
//
// Items from bots count only alongside something else, as on GitHub: a bot's
// status summary on its own is not a reason to wake an agent.
func Delta(items []Item, checkpoint []string) []Item {
	seen := make(map[string]bool, len(checkpoint))
	for _, key := range checkpoint {
		seen[key] = true
	}
	var fresh []Item
	human := false
	for _, item := range items {
		if seen[item.Key] {
			continue
		}
		fresh = append(fresh, item)
		if !item.Bot {
			human = true
		}
	}
	if !human {
		return nil
	}
	return fresh
}

// Signature identifies a set of items independent of order.
func Signature(items []Item) string {
	keys := make([]string, 0, len(items))
	for _, item := range items {
		keys = append(keys, item.Key)
	}
	sort.Strings(keys)
	sum := sha256.Sum256([]byte(strings.Join(keys, "\n")))
	return hex.EncodeToString(sum[:12])
}

// maxCheckpointKeys bounds the checkpoint. The oldest keys go first; a key
// that drops out belongs to feedback old enough that it is no longer present.
const maxCheckpointKeys = 500

// mergeCheckpoint adds keys to a checkpoint, keeping it bounded.
func mergeCheckpoint(checkpoint []string, items []Item) []string {
	seen := make(map[string]bool, len(checkpoint))
	for _, key := range checkpoint {
		seen[key] = true
	}
	for _, item := range items {
		if !seen[item.Key] {
			checkpoint = append(checkpoint, item.Key)
			seen[item.Key] = true
		}
	}
	if len(checkpoint) > maxCheckpointKeys {
		checkpoint = append([]string(nil), checkpoint[len(checkpoint)-maxCheckpointKeys:]...)
	}
	return checkpoint
}
