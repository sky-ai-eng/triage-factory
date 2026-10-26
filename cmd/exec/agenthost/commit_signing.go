package agenthost

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/commitsigner"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/githooks"
	"github.com/sky-ai-eng/triage-factory/internal/gitproxy"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// LocalCommitSigning is what a local run's commits are signed with beyond the
// run's identity. The spawner sets it on a run whose org has an active signer
// when the run starts, and on no other run, so git is configured to sign
// exactly when this exists.
type LocalCommitSigning struct {
	// ClaimID and BootEpoch are the engagement the signatures are made for: a
	// released claim signs nothing, and the per-claim budget counts by it.
	ClaimID   string `json:"claim_id"`
	BootEpoch int64  `json:"boot_epoch"`

	// Signer is the identity the run was started with: the user.name and
	// user.email its git config stamps, and the signer they belong to.
	Signer commitsigner.Identity `json:"signer"`

	// Budgets are the limits read at boot, carried here so a signature made
	// in the exec CLI counts against the same numbers as one the server's
	// daemon makes.
	Budgets commitsigner.Budgets `json:"budgets"`

	// AllowedSignersFile is the run's gpg.ssh.allowedSignersFile. A signature
	// made with a key it does not list yet adds the key, so git verifies the
	// commits a run signs after a rotation it did not start with.
	AllowedSignersFile string `json:"allowed_signers_file"`
}

// CommitSigningEnvVar carries a LocalCommitSigning, as JSON, to the exec CLI
// of a local run with the local sandbox off. That CLI serves the run's verbs
// in-process, with no daemon to be handed it. A run with the sandbox on is
// served by a daemon the spawner hands it directly, and its CLI never reads
// this.
//
// The agent of a run with the sandbox off runs as the operator's user, in
// this environment, so it can run the CLI with a value of its own. The claim,
// the budgets and the committer named here then hold only for an agent that
// does not. Its signer is still checked against the org's, so a value naming
// another account signs nothing.
const CommitSigningEnvVar = "TRIAGE_FACTORY_COMMIT_SIGNING"

// EnvEntry is s as the KEY=VALUE environment entry CommitSigningEnvVar names.
func (s LocalCommitSigning) EnvEntry() (string, error) {
	raw, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("agenthost: encode the run's commit signing: %w", err)
	}
	return CommitSigningEnvVar + "=" + string(raw), nil
}

// commitSigningFromEnv reads CommitSigningEnvVar: nil when it is unset, which
// is a run that does not sign.
func commitSigningFromEnv() (*LocalCommitSigning, error) {
	raw := os.Getenv(CommitSigningEnvVar)
	if raw == "" {
		return nil, nil
	}
	var s LocalCommitSigning
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return nil, fmt.Errorf("agenthost: read %s: %w", CommitSigningEnvVar, err)
	}
	return &s, nil
}

// SignCommitResult is SignCommit's answer: the armored signature git writes
// into the commit, or, when none was made, the one line the run's signing
// program prints in its place (commitsigner.RefusalLine).
type SignCommitResult struct {
	Signature string `json:"signature,omitempty"`
	Refusal   string `json:"refusal,omitempty"`
}

// SetCommitSigning sets what the client's SignCommit signs with, for a client
// that serves a local run's verbs in-process. A client whose effects relay
// (the executor's sidecar) ignores it. err is a CommitSigningEnvVar that could
// not be read: every signature then fails, rather than the run being told it
// does not sign.
func (c *LocalClient) SetCommitSigning(s *LocalCommitSigning, err error) {
	if dr, ok := c.rt.(*directRuntime); ok {
		dr.commitSigning, dr.commitSigningErr = s, err
	}
}

// SignCommit signs one commit object git handed the run's signing program, as
// the run's signer, or returns why not. Every check is made where the run's
// state lives: the runtime's.
func (c *LocalClient) SignCommit(ctx context.Context, payload []byte) (SignCommitResult, error) {
	return c.rt.SignCommit(ctx, payload)
}

// SignCommit makes the signature in this process, with the org's key from the
// secret store: local mode has no control plane to ask.
func (r *directRuntime) SignCommit(ctx context.Context, payload []byte) (SignCommitResult, error) {
	if r.commitSigningErr != nil {
		return SignCommitResult{}, r.commitSigningErr
	}
	cs := r.commitSigning
	if cs == nil {
		// A run git was configured not to sign in, asked to anyway.
		cs = &LocalCommitSigning{}
	}
	signer := commitsigner.LocalSigner{Stores: r.stores, Budgets: cs.Budgets}
	res := signer.Sign(ctx, commitsigner.LocalRequest{
		OrgID:          r.info.OrgID,
		ConversationID: r.info.ConversationID,
		ClaimID:        cs.ClaimID,
		BootEpoch:      cs.BootEpoch,
		Signer:         cs.Signer,
		Payload:        payload,
	})
	if res.Outcome == commitsigner.OutcomeSigned {
		if cs.AllowedSignersFile != "" {
			if err := githooks.AddAllowedSigner(cs.AllowedSignersFile, cs.Signer.Email, res.PublicKey); err != nil {
				agenthostLog.Warn("adding the signing key to the run's allowed-signers file failed; git in the run reports this commit's signature unverified",
					"org", r.info.OrgID, "conversation", r.info.ConversationID, "error", err)
			}
		}
		return SignCommitResult{Signature: res.Signature}, nil
	}
	return SignCommitResult{Refusal: commitsigner.RefusalLine(res, cs.Signer)}, nil
}

// SignCommit on the sidecar answers that the run does not sign: no executor
// run is configured to sign yet.
//
// TODO(TFAC-1198): make the daemon's check here, relay the digest as
// sign_digest, compute the commit id from the signature that comes back and
// relay it to the signature's row.
func (r *relayRuntime) SignCommit(context.Context, []byte) (SignCommitResult, error) {
	res := commitsigner.Result{Outcome: commitsigner.OutcomeRefused, Refusal: commitsigner.RefusalNotSigning}
	return SignCommitResult{Refusal: commitsigner.RefusalLine(res, commitsigner.Identity{Mode: domain.CommitSigningNone})}, nil
}

// signCommitArgs is the SignCommit IPC method's payload: the commit object, as
// git wrote it to its buffer file. The result is SignCommitResult.
type signCommitArgs struct {
	Payload []byte `json:"payload"`
}

func (c *IPCClient) SignCommit(ctx context.Context, payload []byte) (SignCommitResult, error) {
	var res SignCommitResult
	if err := c.call(ctx, methodSignCommit, signCommitArgs{Payload: payload}, &res); err != nil {
		return SignCommitResult{}, err
	}
	return res, nil
}

// pushSigner is a push's detail_json.signer: the login that signed sha, the
// tip the push landed or tried to land, or "unsigned". It is "" when the
// signature log cannot be read here, which leaves the field off rather than
// guessing; the push already happened and its row is written either way. It
// also counts the push in tf_commit_signing_pushes_total when the org signs.
func pushSigner(ctx context.Context, stores db.Stores, orgID, sha string) string {
	if stores.CommitSignatures == nil || sha == "" {
		return ""
	}
	signer, signed, err := commitsigner.TipSigner(ctx, stores, orgID, sha)
	if err != nil {
		agenthostLog.Warn("reading the signature of a pushed commit failed; its audit row names no signer",
			"org", orgID, "sha", sha, "error", err)
		return ""
	}
	countSigningPush(ctx, stores, orgID, signed)
	return signer
}

// MarkPushedSignature stamps the signature that produced a landed push's new
// tip with the repository and the push time, when one of the org's
// signatures produced it, so the control plane's post-push check reads the
// commit's verification on its next GitHub cycle. The first push of a commit
// is the one kept. Host-side only, like RecordGitPushFailed: the git proxy's
// push observer calls it for a branch the upstream accepted. Best-effort: the
// push already happened, so a failure is logged and the commit goes
// unchecked.
func (c *LocalClient) MarkPushedSignature(ctx context.Context, push gitproxy.PushedRef) {
	if c.stores.CommitSignatures == nil || push.NewSHA == "" || !push.Succeeded() {
		return
	}
	sig, err := c.stores.CommitSignatures.ForCommitSystem(ctx, c.info.OrgID, push.NewSHA)
	if err != nil {
		agenthostLog.Warn("reading the signature of a pushed commit failed; the post-push check will not read it",
			"org", c.info.OrgID, "sha", push.NewSHA, "error", err)
		return
	}
	if sig == nil {
		return
	}
	owner, repo, _ := strings.Cut(push.Repo, "/")
	row, err := c.registryRepo(ctx, owner, repo)
	if err == nil && row == nil {
		err = fmt.Errorf("%w: %s", errNoRegistryRow, push.Repo)
	}
	if err != nil {
		agenthostLog.Warn("resolving a pushed commit's repository failed; the post-push check will not read it",
			"org", c.info.OrgID, "sha", push.NewSHA, "error", err)
		return
	}
	if _, err := c.stores.CommitSignatures.MarkPushedSystem(ctx, c.info.OrgID, sig.ID, row.ID); err != nil {
		agenthostLog.Warn("stamping a pushed commit's signature failed; the post-push check will not read it",
			"org", c.info.OrgID, "sha", push.NewSHA, "error", err)
	}
}

// countSigningPush counts a push in tf_commit_signing_pushes_total when the
// org resolves to a signer that signs.
//
// TODO(TFAC-1198): a multi-mode push is recorded on the executor, which cannot
// read the org's signer; count the pushes of runs whose bundle names one.
func countSigningPush(ctx context.Context, stores db.Stores, orgID string, signed bool) {
	if runmode.Current() != runmode.ModeLocal || stores.OrgCommitSigners == nil {
		return
	}
	id, err := commitsigner.Resolver{Signers: stores.OrgCommitSigners}.Resolve(ctx, orgID)
	if err != nil || !id.Mode.Signs() {
		return
	}
	commitsigner.CountPush(ctx, orgID, signed)
}

// withPushSigner is a push row's detail_json, a JSON object, with signer
// added: detailJSON unchanged when signer is "" or detailJSON is not an object.
func withPushSigner(detailJSON, signer string) string {
	if signer == "" {
		return detailJSON
	}
	fields := map[string]json.RawMessage{}
	if strings.TrimSpace(detailJSON) != "" {
		if err := json.Unmarshal([]byte(detailJSON), &fields); err != nil {
			return detailJSON
		}
	}
	raw, _ := json.Marshal(signer)
	fields["signer"] = raw
	out, err := json.Marshal(fields)
	if err != nil {
		return detailJSON
	}
	return string(out)
}
