// Package forgejo implements the ports the features declare against a Forgejo
// or Gitea instance. It owns every provider quirk: which surface answers on
// which release, what a work-in-progress title looks like, how a webhook is
// signed, how a refused write is worded. The wire calls themselves are in
// forgejo/rest; this package maps their types onto the features' own.
package forgejo
