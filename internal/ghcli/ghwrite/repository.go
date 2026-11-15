package ghwrite

import (
	"path"
	"strings"
)

// Named repositories. The injector's repository gate refuses a request that
// names a repository outside the run's authorized set, and this file names
// the repository a REST request addresses; ParseGraphQLRead names the ones a
// GraphQL query selects at its top level (AddressedReads.Repositories). Like
// the rest of this package it decides nothing: what the run may name, and
// what GitHub calls a name, are the injector's.

// RepoRef is one repository a request names, by owner and name as the request
// spelled them.
type RepoRef struct {
	Owner string
	Repo  string
}

// String is the owner/repo spelling.
func (r RepoRef) String() string { return r.Owner + "/" + r.Repo }

// Key identifies the repository among the ones a run has named. It folds
// case: GitHub's names are case-insensitive.
func (r RepoRef) Key() string { return strings.ToLower(r.String()) }

// Valid reports whether both halves have the shape of a repository's owner
// and name (validRepoPart). A name outside it is no repository GitHub serves,
// and is never put into the path of a request the injector makes.
func (r RepoRef) Valid() bool { return validRepoPart(r.Owner) && validRepoPart(r.Repo) }

// reposCollection is the segment every repository-scoped REST path enters
// through.
const reposCollection = "repos"

// NamedRepository names the repository a REST path addresses: the owner and
// name of /repos/{owner}/{repo} and of every path under it, with or without
// the GHE /api/v3 prefix `gh` emits, which the injector strips before it
// forwards. ok is false for a path outside that collection, which names no
// repository here; ref may still fail Valid.
//
// The path is resolved the way the upstream's router resolves it, dot
// segments included, so a spelling that resolves into the collection is read
// as the repository it reaches. A dot segment that climbs past the path's
// root resolves differently under an API served at the host's root and one
// served under /api/v3, so both are tried; where they differ, at most one
// names a repository. The collection is matched without regard to case, which
// can only name a repository the upstream would not route.
func NamedRepository(requestPath string) (ref RepoRef, ok bool) {
	rest := strings.TrimPrefix(requestPath, gheRESTPrefix)
	for _, resolved := range []string{
		path.Clean("/" + rest),
		strings.TrimPrefix(path.Clean(gheRESTPrefix+"/"+rest), gheRESTPrefix),
	} {
		segs := strings.Split(strings.TrimPrefix(resolved, "/"), "/")
		if len(segs) >= 3 && strings.EqualFold(segs[0], reposCollection) && segs[1] != "" && segs[2] != "" {
			return RepoRef{Owner: segs[1], Repo: segs[2]}, true
		}
	}
	return RepoRef{}, false
}
