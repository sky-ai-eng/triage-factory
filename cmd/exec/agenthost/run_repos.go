package agenthost

import (
	"context"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// The repositories a run's GitHub credential is minted for. One function
// answers it for the two places that must agree: the provisioner, which mints
// the run's tokens over this set (internal/cred/credprovision), and the gate a
// relayed read's touch passes (runMayReadRepository), which admits a
// repository only when the run's credential could have read it.

// AuthorizedRepositories returns the conversation's authorized repository set as
// registry rows: every distinct repository in the conversation_worktrees ledger
// of the conversation and of the rest of its task's conversations, PLUS the
// task's own primary repository — all filtered to what the team tracks. The
// conversation_worktrees half is the credential-minting mirror of
// gitAuthorizeDecision (internal/delegate/spawner.go), which enforces the
// same intersection live at the git proxy; minting outside that set would
// be pointless (the proxy would 403 it anyway). The task-repo half exists
// because provisioning happens BEFORE the conversation's very first clone —
// conversation_worktrees has no rows yet for a fresh claim, only for a resumed
// conversation or one that already cloned before a refresh — so without it, a
// fresh conversation's initial host-side clone (setupGitHub's clone auth,
// before any worktree exists) would get no token at all.
//
// Every input already names its repository by registry id — the task's entity
// (entities.repository_id), each ledger row (repository_id) — so the set is
// built, deduplicated and checked against the team's tracking
// (TracksRepositorySystem) by id. A row's name is read only where a token is
// minted for it, which is the name it carries now.
//
// A conversation with no GitHub anchor — a Slack mention, a Jira issue, a taskless
// conversation — has neither a GitHub task-repo nor any worktree, so the set above is
// empty and its bundle would carry no GitHub credential at all: it couldn't
// read a PR diff, check a CI status, or `workspace add` a repo. Such a conversation
// falls back to the team's whole tracked set so it can reach the repos its
// team operates on. This widens READ/API reach, not push authority: the
// tokens stay per-repo scoped (credprovision's resolveGitHub), and pushes remain gated by the
// conversation_worktrees ledger a `workspace add` creates. The boundary is the team's
// own tracked repos — never another team's, never another org's.
//
// Every repository is checked against the team's tracking on the org's
// current GitHub host, the host the bundle's tokens are minted on: a
// repository the team tracked on a host the org has left is not one this
// bundle can reach. A task or checkout on another host is dropped, and it
// still anchors the conversation, so a conversation anchored only on another
// host gets no repositories rather than the team's whole tracked set. A
// retired repository is dropped the same way: a token minted for its name
// would reach the repository GitHub serves under that name now.
func AuthorizedRepositories(ctx context.Context, stores db.Stores, orgID, teamID, taskID, conversationID string) ([]domain.Repository, error) {
	if stores.TeamGitHubRepos == nil {
		return nil, nil
	}
	host, err := db.OrgGitHubHostSystem(ctx, stores.Orgs, orgID)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []domain.Repository
	// anchoredElsewhere records an anchor this bundle cannot reach: a row on
	// another host, a retired row, or a row that cannot be read.
	anchoredElsewhere := false
	// A tracking read that fails is an error, never "untracked": read as
	// untracked it would leave the set empty and hand an anchored run the
	// team's whole tracked set below.
	add := func(row *domain.Repository) error {
		if row == nil || row.Host != host || row.RetiredAt != nil {
			anchoredElsewhere = true
			return nil
		}
		if seen[row.ID] {
			return nil
		}
		tracks, err := stores.TeamGitHubRepos.TracksRepositorySystem(ctx, teamID, row.ID)
		if err != nil {
			return fmt.Errorf("check team tracking of repository %s: %w", row.ID, err)
		}
		if !tracks {
			return nil
		}
		seen[row.ID] = true
		out = append(out, *row)
		return nil
	}

	if row := TaskGitHubRepository(ctx, stores, orgID, taskID); row != nil {
		if err := add(row); err != nil {
			return nil, err
		}
	}
	if stores.ConversationWorktrees != nil {
		rows, err := stores.ConversationWorktrees.ListSystem(ctx, orgID, conversationID)
		if err != nil {
			return nil, err
		}
		// The rest of the task's conversations' checkouts too: they share one
		// run tree, so this conversation can be the one that restores them from
		// the task's snapshot — whose rebuild runs through this bundle — and the
		// bundle is sealed before it records them as its own.
		if taskID != "" {
			taskRows, err := stores.ConversationWorktrees.ListForTaskSystem(ctx, orgID, taskID)
			if err != nil {
				return nil, err
			}
			rows = append(rows, taskRows...)
		}
		for _, w := range rows {
			if err := add(repositoryRow(ctx, stores, orgID, w.RepositoryID)); err != nil {
				return nil, err
			}
		}
	}

	// Unanchored conversation: no task-repo, no worktree. Grant the team's tracked set
	// (already the tracking source of truth, so no per-repo TracksRepositorySystem
	// re-check) rather than shipping a bundle with no GitHub credential at all.
	if len(out) == 0 && !anchoredElsewhere {
		tracked, err := stores.TeamGitHubRepos.ListForTeamSystem(ctx, teamID, host)
		if err != nil {
			return nil, fmt.Errorf("list team tracked repos: %w", err)
		}
		for _, t := range tracked {
			// A retired row's name belongs to another repository now, one the
			// team never chose to track.
			if t.RetiredAt != nil {
				continue
			}
			out = append(out, domain.Repository{ID: t.RepositoryID, ExternalID: t.ExternalID, Source: domain.RepoSourceGitHub, Host: host, Owner: t.Owner, Repo: t.Repo})
		}
	}
	return out, nil
}

// repositoryRow is the registry row id names, or nil when there is no id or
// the read fails: the caller treats an unreadable row as one it cannot reach.
func repositoryRow(ctx context.Context, stores db.Stores, orgID, id string) *domain.Repository {
	if stores.Repos == nil || id == "" {
		return nil
	}
	row, err := stores.Repos.GetSystem(ctx, orgID, id)
	if err != nil {
		return nil
	}
	return row
}

// TaskGitHubRepository resolves a GitHub task's own repository: the registry row the
// task's entity references (entities.repository_id), read by that id, so the
// name is the repository's now whatever the entity's key still spells. nil for
// a non-GitHub task, whose entity references no repository, or when any read
// fails.
func TaskGitHubRepository(ctx context.Context, stores db.Stores, orgID, taskID string) *domain.Repository {
	if stores.Tasks == nil || stores.Entities == nil || taskID == "" {
		return nil
	}
	task, err := stores.Tasks.GetSystem(ctx, orgID, taskID)
	if err != nil || task == nil || task.EntitySource != "github" {
		return nil
	}
	entity, err := stores.Entities.GetSystem(ctx, orgID, task.EntityID)
	if err != nil || entity == nil {
		return nil
	}
	return repositoryRow(ctx, stores, orgID, entity.RepositoryID)
}
