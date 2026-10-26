package forgejo

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"kandev-plugin-forgejo/internal/forgejo/rest"
	"kandev-plugin-forgejo/internal/prautomation"
	"kandev-plugin-forgejo/internal/scm"
)

func automationReviews(reviews []rest.Review) []prautomation.Review {
	var observed []prautomation.Review
	for _, review := range reviews {
		if state := automationReviewState(review.State); state != "" {
			observed = append(observed, prautomation.Review{
				ID:        review.ID,
				Author:    review.User.Login,
				Bot:       isBotUser(review.User),
				State:     state,
				Official:  review.Official,
				Dismissed: review.Dismissed,
				Body:      review.Body,
			})
		}
	}
	return observed
}

func automationComments(comments []rest.IssueComment) []prautomation.Comment {
	var observed []prautomation.Comment
	for _, comment := range comments {
		observed = append(observed, prautomation.Comment{
			ID:     comment.ID,
			Author: comment.User.Login,
			Bot:    isBotUser(comment.User),
			Body:   comment.Body,
		})
	}
	return observed
}

// observeReviewComments fills in the inline comments of the observed reviews,
// which are in the order of the raw ones they came from, and reports whether
// it read them all. They are fetched per review, newest first: the bound keeps
// a long review history from costing a request per review, and the newest
// reviews are the ones still open. Reviews past the bound are reported, so
// auto-merge does not take an unread comment for a resolved one. A dismissed
// review's comments are read too: a conversation it started carries the
// resolver for replies filed under other, live reviews.
func observeReviewComments(ctx context.Context, client *rest.Client, owner, name string, number int64, raw []rest.Review, observed []prautomation.Review) (bool, error) {
	byID := make(map[int64]int, len(observed))
	for i, review := range observed {
		byID[review.ID] = i
	}
	type placed struct {
		review, comment int
		id              int64
		thread          string
		resolved        bool
	}
	var all []placed
	fetched, complete := 0, true
	for i := len(raw) - 1; i >= 0; i-- {
		review := raw[i]
		index, ok := byID[review.ID]
		if !ok || review.CommentsCount == 0 {
			continue
		}
		if fetched == maxReviewCommentFetch {
			complete = false
			break
		}
		fetched++
		comments, err := client.ReviewComments(ctx, owner, name, number, review.ID)
		if errors.Is(err, scm.ErrNotFound) {
			// Deleted since the list was read: it has nothing left to say.
			continue
		}
		if err != nil {
			return false, err
		}
		for _, comment := range comments {
			all = append(all, placed{
				review: index, comment: len(observed[index].Comments), id: comment.ID,
				thread:   fmt.Sprintf("%s\x00%d\x00%d", comment.Path, comment.Position, comment.OriginalPosition),
				resolved: comment.Resolver != nil && comment.Resolver.ID != 0,
			})
			observed[index].Comments = append(observed[index].Comments, prautomation.InlineComment{
				ID:     comment.ID,
				Author: comment.User.Login,
				Bot:    isBotUser(comment.User),
				Path:   comment.Path,
				Body:   comment.Body,
			})
		}
	}
	// A conversation is resolved when its first comment is, and so is every
	// reply in it. It is grouped by place, not by review, as the web UI does:
	// Gitea files a reply under the replier's pending review when there is one.
	first := map[string]placed{}
	for _, comment := range all {
		if current, ok := first[comment.thread]; !ok || comment.id < current.id {
			first[comment.thread] = comment
		}
	}
	for _, comment := range all {
		observed[comment.review].Comments[comment.comment].Resolved = first[comment.thread].resolved
	}
	return complete, nil
}

func automationReviewState(state string) string {
	switch strings.ToUpper(strings.TrimSpace(state)) {
	case "APPROVED":
		return prautomation.ReviewApproved
	case "REQUEST_CHANGES", "CHANGES_REQUESTED":
		return prautomation.ReviewChangesRequested
	case "COMMENT", "COMMENTED":
		return prautomation.ReviewCommented
	default:
		// PENDING (an unsubmitted draft review) and REQUEST_REVIEW are not
		// reviews anyone wrote.
		return ""
	}
}

// isBotUser recognises the accounts the instance itself posts as: the Actions
// user and the ghost user have negative ids on both hosts. Neither host marks
// other bots; a "[bot]" login suffix is the common convention.
func isBotUser(user rest.User) bool {
	login := strings.ToLower(strings.TrimSpace(user.Login))
	return user.ID < 0 || strings.HasSuffix(login, "[bot]") ||
		login == "forgejo-actions" || login == "gitea-actions"
}
