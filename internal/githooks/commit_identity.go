package githooks

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

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
// whose delegating human is not the org's account, the co-author trailer) from
// the resolved account + email. Pure — no I/O — so it's the single,
// unit-testable seam the delegate threads both run modes through.
//
//   - org is the org's GitHub identity: login "<login>" for a PAT org,
//     "<slug>[bot]" for an App org, with the PAT owner's or the bot's numeric id
//     when stored. An empty login → (zero, false): nothing to stamp, and the
//     caller must leave git identity unset (inherit ambient — never a
//     fabricated identity). The login is the commit author name.
//   - orgEmail is the author/committer email, already in its final form. The
//     App-vs-PAT decision lives in the resolver (which has the data): a PAT org
//     gets the credential owner's verified primary email; an App org gets the numeric-id
//     noreply form "<bot_user_id>+<slug>[bot]@users.noreply.github.com" when the
//     bot user id is known (the form that links a bot's commits on github.com),
//     else the plain "<slug>[bot]@..." form. This function passes it through
//     verbatim.
//   - The Co-authored-by trailer is emitted iff the run is MANUAL, the
//     delegating user's login is known, AND the user is not the org's account
//     (domain.GitHubAccount.Is: by id when both are known, else by login). The N=1
//     same-PAT case gets no trailer — a self co-author GitHub would dedupe
//     anyway; a human never is the "<slug>[bot]" account, so manual App runs
//     always co-attribute. The trailer's own email is always the plain
//     "<login>@users.noreply.github.com" form (the human is a user account — the
//     plain form links; the numeric form is never applied here).
//     Autonomous/event runs (manual=false) get the org identity only.
func ResolveCommitIdentity(org domain.GitHubAccount, orgEmail string, manual bool, user domain.GitHubAccount) (CommitIdentity, bool) {
	if org.Login == "" {
		return CommitIdentity{}, false // nothing to stamp
	}
	id := CommitIdentity{Name: org.Login, Email: orgEmail}
	if manual && user.Login != "" && !org.Is(user.Login, user.ID) {
		id.CoAuthorTrailer = "Co-authored-by: " + user.Login + " <" + user.Login + "@users.noreply.github.com>"
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

// SigningConfigPairs returns the git config that has every commit a run makes
// signed by the run's signer, in the shape IdentityConfigPairs produces so both
// feed the one GIT_CONFIG_* block a spawn assembles:
//
//   - gpg.format=ssh and commit.gpgsign=true: sign every commit, with SSH.
//   - user.signingkey=key::<publicKey>: the signer's public key, which git
//     hands the signing program and which a run's own signature check reads.
//     Git does not compare the key that signs with it.
//   - gpg.ssh.program=<hooksDir>/ssh-sign: the shim that hands the signing
//     call to the run's daemon. hooksDir is the hooks dir where the run sees
//     it (HostDir in a local run, SandboxDir in a jail). Git needs the path
//     absolute, because it runs commits from the repository's top directory.
//   - gpg.ssh.allowedSignersFile=<allowedSigners>: the run's allowed-signers
//     file (AllowedSigners), so git verify-commit and git log
//     --show-signature can check the run's own commits.
//
// Tags and pushes are not signed: tag.gpgsign and push.gpgsign stay unset.
func SigningConfigPairs(hooksDir, publicKey, allowedSigners string) [][2]string {
	return [][2]string{
		{"gpg.format", "ssh"},
		{"commit.gpgsign", "true"},
		{"user.signingkey", "key::" + publicKey},
		{"gpg.ssh.program", filepath.Join(hooksDir, SignProgram)},
		{"gpg.ssh.allowedSignersFile", allowedSigners},
	}
}

// AllowedSigners is the content of a run's allowed-signers file: one line per
// public key, `<email> namespaces="git" <key>`, for the signer's key and,
// during a rotation, its pending key, so a run that started before the new key
// was verified still checks the commits it signs with it. Empty keys are
// skipped. The file holds only public values.
func AllowedSigners(email string, publicKeys ...string) []byte {
	var b strings.Builder
	for _, key := range publicKeys {
		if key == "" {
			continue
		}
		b.WriteString(email + ` namespaces="git" ` + key + "\n")
	}
	return []byte(b.String())
}

// AddAllowedSigner appends publicKey's line to the allowed-signers file at
// path when the file does not list it yet: a key the signer rotated to after
// the run started, which git then checks the run's later commits against.
// The file is appended to in place, never replaced, because the local sandbox
// binds it by inode.
func AddAllowedSigner(path, email, publicKey string) error {
	line := AllowedSigners(email, publicKey)
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(line) == 0 || bytes.Contains(content, line) {
		return nil
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	if _, err := f.Write(line); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
