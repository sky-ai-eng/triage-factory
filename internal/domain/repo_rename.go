package domain

import (
	"net/url"
	"strings"
)

// RepoRenameOutcome is what a rename attempt did. Renamed is false for every
// no-op — the repository has no row, the provider reports the slug TF already
// stores, or the caller had no provider id to key on — and those are outcomes
// rather than errors: a rename that has already been applied is the steady
// state, and a second attempt observing it is exactly right.
//
// From/To are set only when Renamed is true. From is the slug as it was
// stored, with its original casing, so a log line can name what moved.
type RepoRenameOutcome struct {
	Renamed bool
	From    string
	To      string
}

// SameRepoSlug reports whether two "owner/repo" slugs name the same
// repository. GitHub identifiers are case-insensitive, so this is the
// comparison every slug lookup in the stores already performs, and it is the
// reason a casing-only rename ("Acme/Api" → "Acme/api") is not a rename here:
// stored casing is sticky, both spellings resolve, and treating it as a move
// would rewrite every reference in the product to say the same thing.
func SameRepoSlug(a, b string) bool { return strings.EqualFold(a, b) }

// DetectRepoRenames returns the observed identities whose repository TF stores
// under a different slug — the rename condition, stated in the only terms that
// can express it: same (source, external id), different (owner, repo).
//
// stored is what TF has, observed is what the provider currently reports.
// Neither side may contribute a match without an external id: a repository
// whose id TF has not learned is never treated as renamed, in either
// direction, because the slug alone cannot tell a rename from a deletion
// followed by a new repository claiming the freed name. That case — the one
// that corrupts data if detection keys on the wrong thing — falls out of the
// same rule: the new repository's id differs, so it matches no stored row and
// no rename is reported.
//
// The result carries the observed refs, not a diff: applying a rename re-reads
// and re-decides under a row lock, so what this returns is a candidate list,
// never an instruction.
func DetectRepoRenames(stored, observed []RepoRef) []RepoRef {
	if len(stored) == 0 || len(observed) == 0 {
		return nil
	}
	// Key on (source, external id): an id is only an identity within the
	// provider that issued it.
	type identity struct{ source, externalID string }
	slugs := make(map[identity]string, len(stored))
	for _, s := range stored {
		if s.ExternalID == "" {
			continue
		}
		source, err := NormalizeRepoSource(s.Source)
		if err != nil {
			continue
		}
		slugs[identity{source, s.ExternalID}] = s.Slug()
	}

	var out []RepoRef
	for _, o := range observed {
		if o.ExternalID == "" {
			continue
		}
		source, err := NormalizeRepoSource(o.Source)
		if err != nil {
			continue
		}
		was, ok := slugs[identity{source, o.ExternalID}]
		if !ok || SameRepoSlug(was, o.Slug()) {
			continue
		}
		o.Source = source
		out = append(out, o)
	}
	return out
}

// RewriteRepoSlugPrefix rewrites a leading repository slug in a slug-prefixed
// identifier — an entity's source_id ("owner/repo#18"), an artifact target
// ("owner/repo" or "owner/repo#18"). Reports false when s does not start with
// oldSlug at a boundary, in which case s is returned unchanged.
//
// The boundary is what keeps "octo/api" from matching "octo/api-gateway#3".
// A slug ends at the end of the string or at the delimiter that introduces
// what follows it — '#' for a PR number, ':' for a dedup key's anchor — and
// never mid-token, so a prefix that happens to spell another repository's name
// is not a match.
func RewriteRepoSlugPrefix(s, oldSlug, newSlug string) (string, bool) {
	if oldSlug == "" || len(s) < len(oldSlug) {
		return s, false
	}
	if !strings.EqualFold(s[:len(oldSlug)], oldSlug) {
		return s, false
	}
	rest := s[len(oldSlug):]
	if rest != "" && rest[0] != '#' && rest[0] != ':' {
		return s, false
	}
	return newSlug + rest, true
}

// RewriteArtifactDedupKey rewrites the repository slug inside an artifact
// dedup key. The key is "provider:kind:resource[:anchor]" (ArtifactDedupKey),
// and the slug — when the artifact has one — is the head of resource:
//
//	github:pull_request:owner/repo#123
//	github:review:owner/repo#123:<conversation>
//	git:branch:owner/repo:refs/heads/x
//
// So the rewrite is the resource's leading slug and nothing else. It has to be
// positional rather than a substring replacement: a branch key's anchor may
// repeat the repository's own name ("git:branch:octo/api:refs/heads/octo/api-fix"),
// and rewriting that too would move a ref that never moved.
//
// Reports false for a key with no repository slug in it (jira:issue:PROJ-1,
// github:comment:<id>) or too few segments to have a resource at all.
func RewriteArtifactDedupKey(key, oldSlug, newSlug string) (string, bool) {
	provider, rest, ok := strings.Cut(key, ":")
	if !ok {
		return key, false
	}
	kind, resource, ok := strings.Cut(rest, ":")
	if !ok {
		return key, false
	}
	rewritten, ok := RewriteRepoSlugPrefix(resource, oldSlug, newSlug)
	if !ok {
		return key, false
	}
	return provider + ":" + kind + ":" + rewritten, true
}

// RewriteRepoURL rewrites the repository slug inside a provider web URL —
// "<scheme>://<host>/<owner>/<repo>[/...]". The slug sits mid-path rather than
// at the head of the string, so this cannot share RewriteRepoSlugPrefix: the
// match is positional on the URL's first two path segments and nowhere else,
// because a host ("octo.api") or a later segment (a branch named after the
// repository, ".../tree/octo/api") can spell the slug without referring to it.
//
// Reports false — with s returned unchanged — for anything that is not an
// absolute URL whose path leads with the old slug at a segment boundary. A
// value TF never learned, or one already pointing elsewhere, is left exactly
// as it stands; nothing is ever synthesized from the slug.
func RewriteRepoURL(rawURL, oldSlug, newSlug string) (string, bool) {
	if rawURL == "" || oldSlug == "" {
		return rawURL, false
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return rawURL, false
	}
	p := u.Path
	if len(p) < 1+len(oldSlug) || p[0] != '/' {
		return rawURL, false
	}
	if !strings.EqualFold(p[1:1+len(oldSlug)], oldSlug) {
		return rawURL, false
	}
	rest := p[1+len(oldSlug):]
	if rest != "" && rest[0] != '/' {
		return rawURL, false
	}
	u.Path = "/" + newSlug + rest
	u.RawPath = ""
	return u.String(), true
}
