package tracker

import (
	"strconv"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// WithRefreshMarks has the tracker's GitHub passes honor marks, the org's
// pending refresh marks as the poller read them: a pull request a mark names is
// refreshed whatever the refresh gate says, and a base branch a mark names is
// checked by the branch watch. A mark names a pull request by its number or by
// a head SHA its stored snapshot is at, in the repository the mark is on. A
// mark naming a repository's Dependabot alerts names nothing these passes
// refresh; the poller hands it to the alert listing (AlertListing.Marked).
//
// The poller clears the marks after the passes that honored them return; the
// tracker never clears one.
func (t *Tracker) WithRefreshMarks(marks []domain.GitHubRefreshMark) *Tracker {
	t.marks = indexRefreshMarks(marks)
	return t
}

// refreshMarks is a pass's refresh marks, indexed for Phase 2 and the branch
// watch. The zero value names nothing.
type refreshMarks struct {
	// prs and shas are keyed by registry row id and the mark's target.
	prs, shas map[repoTarget]bool
	branches  map[domain.BranchRef]bool
}

type repoTarget struct{ repositoryID, value string }

func indexRefreshMarks(marks []domain.GitHubRefreshMark) refreshMarks {
	var m refreshMarks
	for _, mark := range marks {
		key := repoTarget{repositoryID: mark.RepositoryID, value: mark.Target.Value}
		switch mark.Target.Kind {
		case domain.GitHubRefreshPullRequest:
			if m.prs == nil {
				m.prs = map[repoTarget]bool{}
			}
			m.prs[key] = true
		case domain.GitHubRefreshHeadSHA:
			if m.shas == nil {
				m.shas = map[repoTarget]bool{}
			}
			m.shas[key] = true
		case domain.GitHubRefreshBranch:
			if m.branches == nil {
				m.branches = map[domain.BranchRef]bool{}
			}
			m.branches[domain.BranchRef{RepositoryID: mark.RepositoryID, Branch: mark.Target.Value}] = true
		}
	}
	return m
}

// names reports whether a mark names e: its number, taken from its key, or
// the head its stored snapshot snap is at. A mark's SHA is lowercase, as the
// receiver stores it, and GitHub writes a head the same way, but the stored
// head is folded all the same.
func (m refreshMarks) names(e domain.Entity, snap domain.PRSnapshot) bool {
	if e.RepositoryID == "" {
		return false
	}
	if n := domain.GitHubPRNumber(e.SourceID); n > 0 && m.prs[repoTarget{e.RepositoryID, strconv.Itoa(n)}] {
		return true
	}
	return snap.HeadSHA != "" && m.shas[repoTarget{e.RepositoryID, strings.ToLower(snap.HeadSHA)}]
}
