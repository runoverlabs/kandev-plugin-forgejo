package sourcecontrol

// PullRequestSummary is one pull request as the agent tools report it.
type PullRequestSummary struct {
	Number int64
	// State is open, closed or merged: merged is told apart from closed, which
	// the provider's own state field does not do. Unknown when it reports none.
	State   string
	URL     string
	Title   string
	Head    string
	Base    string
	HeadSHA string
	// Draft is the work-in-progress title marker.
	Draft bool
}
