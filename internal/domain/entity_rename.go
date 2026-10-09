package domain

import "strings"

// EntityRef is an entity as a rename sees it: the key it is stored or observed
// under (SourceID) and the identity a key change does not move (Source, Scope,
// ExternalID).
type EntityRef struct {
	Source     string
	Scope      string
	SourceID   string
	ExternalID string
}

// EntityRenameOutcome is what an entity rename attempt did. Renamed is false
// for every no-op — no row carries the id, the stored key is already the one
// observed, or the caller had no id to key on — and those are outcomes rather
// than errors, as on RepoRenameOutcome.
//
// EntityID, From, To and PollSeq are set only when Renamed is true. From is
// the key as it was stored, so a log line or an event can name what moved.
// PollSeq is the entity's poll_seq after the rename, which bumped it: the
// renaming caller CASes its snapshot on it, and any other writer still
// holding the version from before the rename misses.
type EntityRenameOutcome struct {
	Renamed  bool
	EntityID string
	From     string
	To       string
	PollSeq  int64
}

// DetectEntityRenames returns the observed refs whose entity TF stores under a
// different key — the rename condition, stated the only way that can express
// it: same (source, scope, external id), different source_id. The entity
// sibling of DetectRepoRenames, under the same rule: neither side contributes
// a match without an external id, because a key alone cannot tell a rename
// from an object deleted and a new one given the freed key. That case falls
// out of the same rule — the new object's id differs, so it matches no stored
// row and no rename is reported.
//
// Keys compare exactly. Every source with an external id answers in one
// canonical spelling (a Linear identifier, a Jira key folded by
// NormalizeJiraKey), so any difference is a different key.
//
// The result carries the observed refs, not a diff: applying a rename
// re-reads and re-decides under a row lock, so what this returns is a
// candidate list, never an instruction.
func DetectEntityRenames(stored, observed []EntityRef) []EntityRef {
	if len(stored) == 0 || len(observed) == 0 {
		return nil
	}
	type identity struct{ source, scope, externalID string }
	keys := make(map[identity]string, len(stored))
	for _, s := range stored {
		if s.ExternalID == "" {
			continue
		}
		keys[identity{s.Source, s.Scope, s.ExternalID}] = s.SourceID
	}

	var out []EntityRef
	for _, o := range observed {
		if o.ExternalID == "" || o.SourceID == "" {
			continue
		}
		was, ok := keys[identity{o.Source, o.Scope, o.ExternalID}]
		if !ok || was == o.SourceID {
			continue
		}
		out = append(out, o)
	}
	return out
}

// RewriteEntityURL moves a link that resolves to an entity's old url onto its
// new one, keeping whatever followed it — a comment's fragment, a query, a
// trailing path segment. It matches only a link that starts with oldURL at a
// boundary (the end, '#', '?' or '/'), so a link to ENG-41 is never mistaken
// for one to ENG-4. Reports false, with u unchanged, for anything else,
// including when either url is unknown.
func RewriteEntityURL(u, oldURL, newURL string) (string, bool) {
	if u == "" || oldURL == "" || newURL == "" || oldURL == newURL || !strings.HasPrefix(u, oldURL) {
		return u, false
	}
	rest := u[len(oldURL):]
	if rest != "" && rest[0] != '#' && rest[0] != '?' && rest[0] != '/' {
		return u, false
	}
	return newURL + rest, true
}
