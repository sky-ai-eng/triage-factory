package domain

import (
	"net/url"
	"strings"
)

// RepoRenameOutcome is what a rename attempt did. Renamed and Restored are
// both false for every no-op — the repository has no row, the provider reports
// the slug TF already stores for a row that holds it, or the caller had no
// provider id to key on — and those are outcomes rather than errors: a rename
// that has already been applied is the steady state, and a second attempt
// observing it is exactly right.
//
// Renamed is whether the stored slug moved. Restored is whether the row was
// retired (Repository.RetiredAt) and holds a name again: the provider answered
// for its id, so it is a live repository, under the slug it reported. A retired
// row observed under its stored slug is Restored without being Renamed.
//
// From/To are set when either is true. From is the slug as it was stored, with
// its original casing, so a log line can name what moved.
type RepoRenameOutcome struct {
	Renamed  bool
	Restored bool
	From     string
	To       string
}

// Changed reports whether the attempt wrote anything.
func (o RepoRenameOutcome) Changed() bool { return o.Renamed || o.Restored }

// RepoIdentity is one stored repository's identity as the detections below
// compare it against what the provider reports: the slug the row is stored
// under, the provider's id, and whether the row is retired. A retired row keeps
// its slug for its history but no longer holds it, so it takes part in the id
// half of a comparison and never in the name half. The ref also carries the
// owner account's id the row stores, which the detections do not compare and a
// resolution of the row (reporename.Resolve) selects its credential by.
type RepoIdentity struct {
	RepoRef
	Retired bool
}

// RenamedOwnerExternalID is the owner account id a repository row carries once
// a rename or a restore moves it from storedSlug to observed's slug, given the
// id it stores now. The id observed carries is written. Without one, the stored
// id stays while the owner login is unchanged (case-insensitively) and is
// cleared when it moved: a transfer changes the owning account, so an id the
// observation does not confirm may name the account the repository left, and
// with no id the owner is matched by the login the rename writes.
func RenamedOwnerExternalID(storedSlug, storedOwnerID string, observed RepoRef) string {
	if observed.OwnerExternalID != "" {
		return observed.OwnerExternalID
	}
	owner, _, _ := strings.Cut(storedSlug, "/")
	if strings.EqualFold(owner, observed.Owner) {
		return storedOwnerID
	}
	return ""
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
// can express it: same (source, host, external id), different (owner, repo).
// An observation of a retired row's id is a candidate whatever slug it
// carries, since the row holds no name until the rename gives it one back.
//
// The host is part of the identity because GitHub repository ids are
// per-deployment sequences: another host's repository can carry the same id,
// and matching across hosts would rename a repository into a stranger's. A ref
// with no host matches nothing, on either side.
//
// stored is what TF has, observed is what the provider currently reports.
// Neither side may contribute a match without an external id: a repository
// whose id TF has not learned is never treated as renamed, in either
// direction, because the slug alone cannot tell a rename from a deletion
// followed by a new repository claiming the freed name. That case — the one
// that corrupts data if detection keys on the wrong thing — falls out of the
// same rule: the new repository's id differs, so it matches no stored row and
// no rename is reported. DetectReassignedRepoNames reports it instead.
//
// The result carries the observed refs, not a diff: applying a rename re-reads
// and re-decides under a row lock, so what this returns is a candidate list,
// never an instruction.
func DetectRepoRenames(stored []RepoIdentity, observed []RepoRef) []RepoRef {
	if len(stored) == 0 || len(observed) == 0 {
		return nil
	}
	type held struct {
		slug    string
		retired bool
	}
	byID := make(map[repoIdentityKey]held, len(stored))
	for _, s := range stored {
		if k, ok := identityKey(s.RepoRef); ok {
			byID[k] = held{slug: s.Slug(), retired: s.Retired}
		}
	}

	var out []RepoRef
	for _, o := range observed {
		k, ok := identityKey(o)
		if !ok {
			continue
		}
		was, ok := byID[k]
		if !ok || (!was.retired && SameRepoSlug(was.slug, o.Slug())) {
			continue
		}
		o.Source = k.source
		out = append(out, o)
	}
	return out
}

// DetectReassignedRepoNames returns the stored identities whose name the
// provider reports under a different id: the repository TF holds under the name
// is not the one the provider now serves there, so it was deleted, or renamed
// away and the name reused. It is the mirror image of DetectRepoRenames — same
// (source, host, folded slug), different external id — and a row it returns has
// to be resolved by its own id (reporename.Resolve) before the name is free.
//
// Only rows that hold their name take part: a retired row has given its up. A
// row whose own id is among the observations is left out too, because the
// observation already says where it is and DetectRepoRenames acts on that. As
// in DetectRepoRenames, neither side contributes without a host and an id.
func DetectReassignedRepoNames(stored []RepoIdentity, observed []RepoRef) []RepoRef {
	if len(stored) == 0 || len(observed) == 0 {
		return nil
	}
	type nameKey struct{ source, host, slug string }
	observedIDs := make(map[repoIdentityKey]bool, len(observed))
	observedNames := make(map[nameKey]string, len(observed))
	for _, o := range observed {
		k, ok := identityKey(o)
		if !ok {
			continue
		}
		observedIDs[k] = true
		observedNames[nameKey{k.source, k.host, strings.ToLower(o.Slug())}] = o.ExternalID
	}

	var out []RepoRef
	for _, s := range stored {
		k, ok := identityKey(s.RepoRef)
		if !ok || s.Retired || observedIDs[k] {
			continue
		}
		id, ok := observedNames[nameKey{k.source, k.host, strings.ToLower(s.Slug())}]
		if !ok || id == s.ExternalID {
			continue
		}
		ref := s.RepoRef
		ref.Source = k.source
		out = append(out, ref)
	}
	return out
}

// repoIdentityKey is a repository's identity as the detections key on it: an
// id is only an identity within the deployment of the provider that issued it.
type repoIdentityKey struct{ source, host, externalID string }

// identityKey keys r, or reports false for a ref that cannot take part in a
// comparison: no id, no host, or a source no provider issues.
func identityKey(r RepoRef) (repoIdentityKey, bool) {
	if r.ExternalID == "" || r.Host == "" {
		return repoIdentityKey{}, false
	}
	source, err := NormalizeRepoSource(r.Source)
	if err != nil {
		return repoIdentityKey{}, false
	}
	return repoIdentityKey{source, r.Host, r.ExternalID}, true
}

// RewriteRepoSlugPrefix rewrites a leading repository slug in a slug-prefixed
// identifier — an entity's source_id ("owner/repo#18"), an artifact target
// ("owner/repo" or "owner/repo#18"). Reports false when s does not start with
// oldSlug at a boundary, in which case s is returned unchanged.
//
// The boundary is what keeps "octo/api" from matching "octo/api-gateway#3".
// A slug ends at the end of the string or at the '#' that introduces a PR
// number, and never mid-token, so a prefix that happens to spell another
// repository's name is not a match.
func RewriteRepoSlugPrefix(s, oldSlug, newSlug string) (string, bool) {
	if oldSlug == "" || len(s) < len(oldSlug) {
		return s, false
	}
	if !strings.EqualFold(s[:len(oldSlug)], oldSlug) {
		return s, false
	}
	rest := s[len(oldSlug):]
	if rest != "" && rest[0] != '#' {
		return s, false
	}
	return newSlug + rest, true
}

// RewriteRepoURL rewrites the repository slug inside a link on host, a
// GitHubHost value: "<host>/<owner>/<repo>[/...]", where host carries the
// scheme, the authority and any context path. The slug is the two path
// segments right after the host's own path, and only those: a host
// ("octo.api"), a context path, or a later segment (a branch named after the
// repository, ".../tree/octo/api") can spell the slug without referring to it.
//
// Reports false — with rawURL returned unchanged — for a link that is not on
// host (GitHubURLOnHost) or whose path does not continue with the old slug at
// a segment boundary. A value TF never learned, or one already pointing
// elsewhere, is left exactly as it stands; nothing is ever synthesized from
// the slug.
func RewriteRepoURL(rawURL, host, oldSlug, newSlug string) (string, bool) {
	if oldSlug == "" || !GitHubURLOnHost(rawURL, host) {
		return rawURL, false
	}
	h, err := url.Parse(host)
	if err != nil {
		return rawURL, false
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL, false
	}
	prefix := strings.TrimRight(h.Path, "/") + "/"
	if !strings.HasPrefix(u.Path, prefix) {
		return rawURL, false
	}
	p := u.Path[len(prefix):]
	if len(p) < len(oldSlug) || !strings.EqualFold(p[:len(oldSlug)], oldSlug) {
		return rawURL, false
	}
	rest := p[len(oldSlug):]
	if rest != "" && rest[0] != '/' {
		return rawURL, false
	}
	u.Path = prefix + newSlug + rest
	u.RawPath = ""
	return u.String(), true
}

// RewriteRepoCloneURL rewrites the repository slug in a clone URL on host, a
// GitHubHost value, in either form GitHub reports one: HTTPS
// ("<host>/<owner>/<repo>.git") or SSH ("git@<hostname>:<owner>/<repo>.git",
// or "ssh://git@<hostname>/<owner>/<repo>.git"). An SSH URL is on host when its
// hostname is host's, since the two are served by the same deployment. The
// path after the host must be the old slug, with or without ".git", which the
// rewrite keeps.
//
// Reports false, with rawURL returned unchanged, for a URL on another host or
// one whose path is not exactly the old slug: as with RewriteRepoURL, a value
// TF never learned is left as it stands.
func RewriteRepoCloneURL(rawURL, host, oldSlug, newSlug string) (string, bool) {
	if oldSlug == "" || newSlug == "" {
		return rawURL, false
	}
	h, err := url.Parse(host)
	if err != nil || h.Hostname() == "" {
		return rawURL, false
	}
	if GitHubURLOnHost(rawURL, host) {
		u, err := url.Parse(rawURL)
		if err != nil {
			return rawURL, false
		}
		prefix := strings.TrimRight(h.Path, "/") + "/"
		if !strings.HasPrefix(u.Path, prefix) {
			return rawURL, false
		}
		p, ok := rewriteClonePath(u.Path[len(prefix):], oldSlug, newSlug)
		if !ok {
			return rawURL, false
		}
		u.Path = prefix + p
		u.RawPath = ""
		return u.String(), true
	}
	if strings.HasPrefix(rawURL, "ssh://") {
		u, err := url.Parse(rawURL)
		if err != nil || !strings.EqualFold(u.Hostname(), h.Hostname()) {
			return rawURL, false
		}
		p, ok := rewriteClonePath(strings.TrimPrefix(u.Path, "/"), oldSlug, newSlug)
		if !ok {
			return rawURL, false
		}
		u.Path = "/" + p
		u.RawPath = ""
		return u.String(), true
	}
	if strings.Contains(rawURL, "://") {
		return rawURL, false
	}
	authority, p, ok := strings.Cut(rawURL, ":")
	if !ok {
		return rawURL, false
	}
	hostname := authority
	if at := strings.LastIndex(hostname, "@"); at >= 0 {
		hostname = hostname[at+1:]
	}
	if !strings.EqualFold(hostname, h.Hostname()) {
		return rawURL, false
	}
	p, ok = rewriteClonePath(p, oldSlug, newSlug)
	if !ok {
		return rawURL, false
	}
	return authority + ":" + p, true
}

// rewriteClonePath replaces a clone URL's path, the old slug with or without a
// ".git" suffix, with the new slug carrying the same suffix.
func rewriteClonePath(p, oldSlug, newSlug string) (string, bool) {
	trimmed := strings.TrimSuffix(p, ".git")
	if !strings.EqualFold(trimmed, oldSlug) {
		return "", false
	}
	return newSlug + p[len(trimmed):], true
}

// GitHubURLOnHost reports whether rawURL is a link on host, a GitHubHost value
// (scheme, authority and any context path, with no trailing slash). It is how a
// stored link is placed on a GitHub deployment before anything in it is read
// as an owner/repo (RewriteRepoURL). The scheme and authority compare
// case-insensitively, as URLs define them. The context path compares exactly:
// GitHubHost keeps it as configured, a server may treat paths case-sensitively,
// and every other host comparison is exact. An empty host or link, or one
// that does not parse as an absolute URL, is on no host.
func GitHubURLOnHost(rawURL, host string) bool {
	if rawURL == "" || host == "" {
		return false
	}
	h, err := url.Parse(host)
	if err != nil || h.Scheme == "" || h.Host == "" {
		return false
	}
	u, err := url.Parse(rawURL)
	if err != nil || !strings.EqualFold(u.Scheme, h.Scheme) || !strings.EqualFold(u.Host, h.Host) {
		return false
	}
	return strings.HasPrefix(u.EscapedPath(), strings.TrimRight(h.EscapedPath(), "/")+"/")
}
