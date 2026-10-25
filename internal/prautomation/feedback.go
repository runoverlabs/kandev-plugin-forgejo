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
// where every run has its own id. Reviews and comments are keyed by their id
// and their text: an edited request is feedback the agent has not seen.
// The operator's own words are never feedback — the agent posts as that
// account, and counting them would loop it on its own replies.
func BuildSnapshot(obs Observation, trust Trust) Snapshot {
	snapshot := Snapshot{Items: blockingItems(obs)}
	filter := newFeedbackFilter(obs, trust)
	for _, review := range obs.Reviews {
		if review.Dismissed || filter.own(review.Author) {
			continue
		}
		if reviewIsFeedback(review) {
			snapshot.add(reviewItem(review), filter.admit(review.Official))
		}
		for _, comment := range review.Comments {
			if comment.Resolved || filter.own(comment.Author) || strings.TrimSpace(comment.Body) == "" {
				continue
			}
			// By its author, not its review: a reply in a thread is stored
			// under the review that opened it, whoever wrote the reply.
			snapshot.add(inlineItem(comment), filter.admitAuthor(comment.Author))
		}
	}
	for _, comment := range obs.Comments {
		if filter.own(comment.Author) || strings.TrimSpace(comment.Body) == "" {
			continue
		}
		snapshot.add(commentItem(comment), filter.admitAuthor(comment.Author))
	}
	return snapshot
}

// add appends item when the trust level admits it, and counts it as ignored
// otherwise.
func (s *Snapshot) add(item Item, admitted bool) {
	if !admitted {
		s.Ignored++
		return
	}
	s.Items = append(s.Items, item)
}

// blockingItems are the failing checks and the conflict on the head.
func blockingItems(obs Observation) []Item {
	var items []Item
	for _, check := range obs.CI.Failing {
		items = append(items, Item{
			Key:   fmt.Sprintf("check:%s@%s", check.Name, obs.HeadSHA),
			Kind:  KindCheck,
			Name:  check.Name,
			State: check.State,
			URL:   check.URL,
		})
	}
	if obs.Mergeable != nil && !*obs.Mergeable {
		items = append(items, Item{
			Key:  "conflict@" + obs.HeadSHA,
			Kind: KindConflict,
		})
	}
	return items
}

func reviewIsFeedback(review Review) bool {
	return review.State == ReviewChangesRequested ||
		(review.State == ReviewCommented && strings.TrimSpace(review.Body) != "")
}

func reviewItem(review Review) Item {
	return Item{
		Key:    textKey("review", review.ID, review.Body),
		Kind:   KindReview,
		Author: review.Author,
		Bot:    review.Bot,
		State:  review.State,
		Body:   review.Body,
	}
}

func inlineItem(comment InlineComment) Item {
	return Item{
		Key:    textKey("inline", comment.ID, comment.Body),
		Kind:   KindInline,
		Author: comment.Author,
		Bot:    comment.Bot,
		Path:   comment.Path,
		Body:   comment.Body,
	}
}

func commentItem(comment Comment) Item {
	return Item{
		Key:    textKey("comment", comment.ID, comment.Body),
		Kind:   KindComment,
		Author: comment.Author,
		Bot:    comment.Bot,
		Body:   comment.Body,
	}
}

// feedbackFilter decides whose words are feedback on one observation.
type feedbackFilter struct {
	operator        string
	trust           Trust
	officialAuthors map[string]bool
}

func newFeedbackFilter(obs Observation, trust Trust) feedbackFilter {
	officialAuthors := map[string]bool{}
	for _, review := range obs.Reviews {
		if review.Official && !review.Dismissed {
			officialAuthors[strings.ToLower(review.Author)] = true
		}
	}
	return feedbackFilter{
		operator:        strings.ToLower(strings.TrimSpace(obs.Operator)),
		trust:           trust,
		officialAuthors: officialAuthors,
	}
}

func (f feedbackFilter) own(author string) bool {
	return f.operator != "" && strings.ToLower(strings.TrimSpace(author)) == f.operator
}

func (f feedbackFilter) admit(official bool) bool {
	switch f.trust {
	case TrustNone:
		return false
	case TrustOfficial:
		return official
	default:
		return true
	}
}

// admitAuthor admits a comment by whether its author left an official review.
func (f feedbackFilter) admitAuthor(author string) bool {
	return f.admit(f.officialAuthors[strings.ToLower(author)])
}

// textKey keys a review or comment by its id and a digest of its text.
func textKey(kind string, id int64, body string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(body)))
	return fmt.Sprintf("%s:%d:%s", kind, id, hex.EncodeToString(sum[:6]))
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

// maxCheckpointKeys bounds the checkpoint. Past half of it, pruneCheckpoint
// drops the keys of feedback no longer on the pull request; the oldest keys go
// first only if that is not enough. Feedback that stays (a comment, an open
// conversation) keeps its key, or it would be sent again as new.
const maxCheckpointKeys = 500

// pruneCheckpoint keeps the checkpoint's keys that are among present, once the
// checkpoint is past half its bound. Below that it is left alone, so feedback
// that drops out briefly (a check re-running) is not taken for new on return.
func pruneCheckpoint(checkpoint []string, present []Item) []string {
	if len(checkpoint) <= maxCheckpointKeys/2 {
		return checkpoint
	}
	keep := make(map[string]bool, len(present))
	for _, item := range present {
		keep[item.Key] = true
	}
	pruned := make([]string, 0, len(present))
	for _, key := range checkpoint {
		if keep[key] {
			pruned = append(pruned, key)
		}
	}
	return pruned
}

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
