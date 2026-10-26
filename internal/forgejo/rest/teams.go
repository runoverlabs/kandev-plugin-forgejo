package rest

import (
	"context"
	"strconv"
)

// maxUserTeams bounds the /user/teams walk.
const maxUserTeams = 200

// Team is a team as the pull request and /user/teams report it.
type Team struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Organization *struct {
		UserName string `json:"username"`
	} `json:"organization"`
}

// ReviewThreadComment is an inline comment of a submitted review.
type ReviewThreadComment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
	Path string `json:"path"`
	// Position is the line on the new side, OriginalPosition on the old; the
	// other is 0. With the path they place the comment's conversation.
	Position         int64 `json:"position"`
	OriginalPosition int64 `json:"original_position"`
	User             User  `json:"user"`
	// Resolver is set on the comment a conversation was resolved from. There
	// is no API to resolve one, only the web UI, which resolves from the
	// conversation's first comment and reads its state from there alone.
	Resolver *User `json:"resolver"`
}

// ReviewComments lists the inline comments of one review.
func (c *Client) ReviewComments(ctx context.Context, owner, name string, number, reviewID int64) ([]ReviewThreadComment, error) {
	var comments []ReviewThreadComment
	path := pullPath(owner, name, number, "/reviews/"+strconv.FormatInt(reviewID, 10)+"/comments")
	if err := c.get(ctx, path, nil, &comments); err != nil {
		return nil, err
	}
	return comments, nil
}

// UserTeams lists the teams the token's account belongs to.
func (c *Client) UserTeams(ctx context.Context) ([]Team, error) {
	teams, _, err := pageAll[Team](ctx, c, "/user/teams", maxUserTeams)
	return teams, err
}
