// Package scm is the source-control vocabulary every layer shares: what a
// repository reference is, and the kinds of failure a provider call can end
// in. It imports nothing but the standard library, so a feature, an adapter
// and the transport can all name the same error without importing each other.
//
// The error texts keep their historical "forgejo:" prefix: they reach the
// plugin log, where an operator searching for an old message should still
// find it.
package scm
