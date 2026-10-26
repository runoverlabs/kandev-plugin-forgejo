package scm

import (
	"fmt"
	"strings"
)

// RepoRef names one repository on the connected instance.
type RepoRef struct {
	Owner string `json:"owner"`
	Name  string `json:"name"`
}

// FullName renders the canonical "owner/name" form.
func (r RepoRef) FullName() string { return r.Owner + "/" + r.Name }

// ParseRepoRef reads an "owner/name" string. It rejects anything with a
// different shape rather than guessing, because a malformed entry would
// otherwise become a 404 on every poll with no obvious cause. It is the one
// definition of a repository reference: watches and automation conditions
// both use it.
func ParseRepoRef(raw string) (RepoRef, error) {
	// A pasted path ("/owner/name/") loses its surrounding slashes; what is
	// left must still be exactly owner/name.
	trimmed := strings.Trim(strings.TrimSpace(raw), "/")
	owner, name, found := strings.Cut(trimmed, "/")
	owner, name = strings.TrimSpace(owner), strings.TrimSpace(name)
	if !found || owner == "" || name == "" || strings.Contains(name, "/") ||
		strings.ContainsAny(owner+name, " \t\r\n") {
		return RepoRef{}, fmt.Errorf("repository %q must be in owner/name form", raw)
	}
	return RepoRef{Owner: owner, Name: name}, nil
}
