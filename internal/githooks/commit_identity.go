package githooks

import "strings"

// CommitIdentity is the git author/committer identity stamped onto a
// delegated-agent run's commits, plus the optional Co-authored-by trailer the
// prepare-commit-msg hook appends on manual runs (TFAC-452).
//
// Name/Email are injected as process-scoped user.name / user.email git config
// (IdentityConfigPairs → the agent's GIT_CONFIG_* env, both run modes), so the
// org's GitHub identity is the author + committer of every commit the agent
// makes — including in dynamically-cloned Jira subdirs the per-worktree config
// never reaches. CoAuthorTrailer, when set, is carried to the hook via the
// TRIAGE_FACTORY_GIT_COAUTHOR_TRAILER run env var; an empty trailer leaves the
// hook a no-op.
type CommitIdentity struct {
	Name            string
	Email           string
	CoAuthorTrailer string
}

// ResolveCommitIdentity computes the org commit identity (and, on a manual run
// whose delegating human differs from the org login, the co-author trailer)
// from login-only inputs. Pure — no I/O — so it's the single, unit-testable
// seam the delegate threads both run modes through.
//
//   - orgLogin is the org's GitHub identity: "<login>" for a PAT org,
//     "<slug>[bot]" for an App org. Empty → (zero, false): nothing to stamp,
//     and the caller must leave git identity unset (inherit ambient — never a
//     fabricated identity).
//   - The author/committer email is always "<orgLogin>@users.noreply.github.com"
//     (the login form; numeric-id noreply is out of scope — it links fine for a
//     PAT and is an acceptable approximation for an App bot).
//   - The Co-authored-by trailer is emitted iff the run is MANUAL, a delegating
//     userLogin is known, AND it differs from orgLogin case-insensitively. The
//     N=1 same-PAT case (orgLogin == userLogin) gets no trailer — a self
//     co-author GitHub would dedupe anyway. Autonomous/event runs (manual=false)
//     get the org identity only.
func ResolveCommitIdentity(orgLogin string, manual bool, userLogin string) (CommitIdentity, bool) {
	if orgLogin == "" {
		return CommitIdentity{}, false // nothing to stamp
	}
	id := CommitIdentity{Name: orgLogin, Email: orgLogin + "@users.noreply.github.com"}
	if manual && userLogin != "" && !strings.EqualFold(userLogin, orgLogin) {
		id.CoAuthorTrailer = "Co-authored-by: " + userLogin + " <" + userLogin + "@users.noreply.github.com>"
	}
	return id, true
}

// IdentityConfigPairs returns the git config (key, value) pairs that stamp the
// org commit-author identity — user.name and user.email — or nil when either is
// unset. Both injection paths fold these into the agent's single GIT_CONFIG_*
// block: the local direct-spawn path (DirectAgentEnv) and the sandbox path
// (proxies.go gitPairs). Returning nil for an unset identity is what preserves
// today's behavior (and the existing hooks-only tests) when no identity
// resolves — the block then carries core.hooksPath alone.
func IdentityConfigPairs(name, email string) [][2]string {
	if name == "" || email == "" {
		return nil
	}
	return [][2]string{
		{"user.name", name},
		{"user.email", email},
	}
}
