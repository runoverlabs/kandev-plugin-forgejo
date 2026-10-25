package sourcecontrol

// Repository holds credential-free provider identity and display/routing data.
// RepositoryID must be the provider's immutable identifier; Name is display only.
type Repository struct {
	ProviderID      string `json:"provider_id"`
	ProviderHost    string `json:"provider_host"`
	ConnectionScope string `json:"provider_scope"`
	RepositoryID    string `json:"repository_id"`
	OwnerOrProject  string `json:"owner_or_project"`
	Name            string `json:"name"`
	CloneURL        string `json:"clone_url"`
	DefaultBranch   string `json:"default_branch,omitempty"`
}

type RepositoryIdentity struct {
	ConnectionScope string `json:"connection_scope"`
	RepositoryID    string `json:"repository_id"`
}

type Branch struct {
	Name      string `json:"name"`
	Commit    string `json:"commit,omitempty"`
	IsDefault bool   `json:"is_default,omitempty"`
}

// ChangeRequestIdentity is the complete immutable association key.
type ChangeRequestIdentity struct {
	ConnectionScope string `json:"connection_scope"`
	RepositoryID    string `json:"repository_id"`
	Number          int64  `json:"number"`
}

type ChangeRequest struct {
	Identity ChangeRequestIdentity `json:"identity"`
	Title    string                `json:"title,omitempty"`
	URL      string                `json:"url"`
}

type CreateChangeRequestInput struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Destination string `json:"destination"`
	Draft       bool   `json:"draft"`
}

type ReviewTaskStatusCheck struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
	URL    string `json:"url,omitempty"`
}

type ReviewTaskReview struct {
	State     string `json:"state"`
	Approved  int    `json:"approved"`
	Required  int    `json:"required,omitempty"`
	Requested int    `json:"requested,omitempty"`
}

type ReviewTaskStatus struct {
	Number             int64                   `json:"number"`
	State              string                  `json:"state"`
	PipelineState      string                  `json:"pipeline_state"`
	Checks             []ReviewTaskStatusCheck `json:"checks"`
	Review             *ReviewTaskReview       `json:"review,omitempty"`
	UnresolvedComments int                     `json:"unresolved_comments,omitempty"`
	UpdatedAt          int64                   `json:"updated_at,omitempty"` // Unix milliseconds.
}

type ReviewSummary struct {
	ProviderID          string            `json:"provider_id"`
	ReviewKey           string            `json:"review_key"`
	Title               string            `json:"title"`
	URL                 string            `json:"url"`
	ConnectionScope     string            `json:"connection_scope"`
	RepositoryID        string            `json:"repository_id"`
	ChangeRequestNumber int64             `json:"change_request_number"`
	State               string            `json:"state"`
	TaskStatus          *ReviewTaskStatus `json:"task_status,omitempty"`
}

type ReviewAssociation struct {
	ProviderID          string `json:"provider_id"`
	TaskID              string `json:"task_id"`
	ReviewKey           string `json:"review_key"`
	ConnectionScope     string `json:"connection_scope"`
	RepositoryID        string `json:"repository_id"`
	ChangeRequestNumber int64  `json:"change_request_number"`
}

type RepositoryCursor struct {
	Remote            string
	AfterRepositoryID string
}

type RepositoryPage struct {
	Repositories []Repository
	Next         RepositoryCursor
}
