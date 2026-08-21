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
// whose delegating human differs from the org name, the co-author trailer) from
// the resolved name + email. Pure — no I/O — so it's the single, unit-testable
// seam the delegate threads both run modes through.
//
//   - orgName is the org's GitHub identity name: "<login>" for a PAT org,
//     "<slug>[bot]" for an App org. Empty → (zero, false): nothing to stamp, and
//     the caller must leave git identity unset (inherit ambient — never a
//     fabricated identity).
//   - orgEmail is the author/committer email, already in its final form. The
//     App-vs-PAT decision lives in the resolver (which has the data): a PAT org
//     gets the credential owner's verified primary email; an App org gets the numeric-id
//     noreply form "<bot_user_id>+<slug>[bot]@users.noreply.github.com" when the
//     bot user id is known (the form that links a bot's commits on github.com),
//     else the plain "<slug>[bot]@..." form (TFAC-474). This function passes it
//     through verbatim.
//   - The Co-authored-by trailer is emitted iff the run is MANUAL, a delegating
//     userLogin is known, AND it differs from orgName case-insensitively. The
//     N=1 same-PAT case (orgName == userLogin) gets no trailer — a self
//     co-author GitHub would dedupe anyway; a human userLogin never equals a
//     "<slug>[bot]" name, so manual App runs always co-attribute. The trailer's
//     own email is always the plain "<userLogin>@users.noreply.github.com" form
//     (the human is a user account — the plain form links; the numeric form is
//     never applied here). Autonomous/event runs (manual=false) get the org
//     identity only.
func ResolveCommitIdentity(orgName, orgEmail string, manual bool, userLogin string) (CommitIdentity, bool) {
	if orgName == "" {
		return CommitIdentity{}, false // nothing to stamp
	}
	id := CommitIdentity{Name: orgName, Email: orgEmail}
	if manual && userLogin != "" && !strings.EqualFold(userLogin, orgName) {
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

// FallbackIdentityName / FallbackIdentityEmail are the neutral author and
// committer a SANDBOXED local run commits under when the org has configured
// no GitHub identity of its own.
//
// Outside the sandbox, an unset org identity correctly leaves git ambient: the
// operator's ~/.gitconfig answers "who are you", and fabricating an identity
// over theirs would be worse than useless. Inside it, that file is masked on
// purpose — it carries their credential helpers and url rewrites, which must
// not leak into an agent's git — so ambient resolves to nothing and every
// commit fails with "please tell me who you are". A neutral fallback is what
// keeps that masking from turning an unconfigured org into a run that cannot
// commit.
//
// The address uses the reserved .invalid TLD (RFC 2606), which is guaranteed
// never to resolve: this identity is a placeholder standing in for an
// unanswered question, and it should not be able to look like a real mailbox.
const (
	FallbackIdentityName  = "Triage Factory Agent"
	FallbackIdentityEmail = "agent@triagefactory.invalid"
)

// FallbackIdentityConfigPairs returns the neutral identity as git config
// pairs, in the same shape IdentityConfigPairs produces so both feed the one
// GIT_CONFIG_* block a spawn assembles.
func FallbackIdentityConfigPairs() [][2]string {
	return IdentityConfigPairs(FallbackIdentityName, FallbackIdentityEmail)
}
