package agenthost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/commitsign"
	"github.com/sky-ai-eng/triage-factory/internal/commitsigner"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/githooks"
	"github.com/sky-ai-eng/triage-factory/internal/proxy/gitproxy"
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

// SetSealedCommitSigner records the signer the run's sealed bundle named, as
// its sidecar reported it ("" for a run that does not sign), for a client
// recording an executor run's pushes: the executor cannot read the org's
// signer, so this is what tells it the run's org signs (countSigningPush).
func (c *LocalClient) SetSealedCommitSigner(mode domain.CommitSigningMode) {
	if dr, ok := c.rt.(*directRuntime); ok {
		dr.sealedSigner = mode
	}
}

// sealedSigner is the signer SetSealedCommitSigner recorded.
func (c *LocalClient) sealedSigner() domain.CommitSigningMode {
	if dr, ok := c.rt.(*directRuntime); ok {
		return dr.sealedSigner
	}
	return ""
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

// SignCommit on the sidecar makes the daemon's check on the commit, relays its
// digest to the orchestrator as sign_digest for a control pod to sign as the
// signer the run was sealed with, and records the commit id the signature
// produces on the signature's row. The sidecar holds no signing key, and the
// control plane never sees the commit: what crosses is the digest one way and
// the signature the other. A run whose bundle named no signer has git
// configured not to sign, and is told so if it asks anyway.
func (r *relayRuntime) SignCommit(ctx context.Context, payload []byte) (SignCommitResult, error) {
	if r.signer == nil || !r.signer.Mode.Signs() {
		res := commitsigner.Result{Outcome: commitsigner.OutcomeRefused, Refusal: commitsigner.RefusalNotSigning}
		return SignCommitResult{Refusal: commitsigner.RefusalLine(res, commitsigner.Identity{Mode: domain.CommitSigningNone})}, nil
	}
	signer := *r.signer
	res := r.signCommit(ctx, signer, payload)
	if res.Outcome == commitsigner.OutcomeSigned {
		return SignCommitResult{Signature: res.Signature}, nil
	}
	return SignCommitResult{Refusal: commitsigner.RefusalLine(res, signer)}, nil
}

// signCommit is SignCommit for a run that signs as signer. Every failure to
// get an answer is OutcomeUnanswered, which tells the run to try again.
func (r *relayRuntime) signCommit(ctx context.Context, signer commitsigner.Identity, payload []byte) commitsigner.Result {
	unanswered := commitsigner.Result{Outcome: commitsigner.OutcomeUnanswered}
	accountID, err := strconv.ParseInt(signer.AccountID, 10, 64)
	if err != nil || accountID <= 0 {
		agenthostLog.Error("the run's sealed signer has no account id; answering its signature unanswered",
			"conversation", r.info.ConversationID, "account", signer.AccountID)
		return unanswered
	}

	res, ok, err := commitsigner.CheckCommit(payload, signer)
	if err != nil {
		agenthostLog.Warn("could not read the commit git asked to have signed; answering it unanswered",
			"conversation", r.info.ConversationID, "error", err)
		return unanswered
	}
	if !ok {
		// The orchestrator counts the refusal and, for a committer mismatch,
		// writes its commit_sign_denied row: this process has no database
		// and no metrics.
		r.conn.record(agentproc.RelayNamespaceCore, agentproc.OpRecordSignRefusal, agentproc.RecordSignRefusalArgs{
			Reason:         string(res.Refusal),
			CommitterName:  res.Committer.Name,
			CommitterEmail: res.Committer.Email,
		})
		return res
	}

	var reply agentproc.SignDigestReply
	if err := r.conn.call(ctx, agentproc.RelayNamespaceCore, agentproc.OpSignDigest, agentproc.SignDigestArgs{
		Digest:       commitsign.DigestOf(payload).String(),
		Signer:       string(signer.Mode),
		SignerUserID: accountID,
	}, &reply); err != nil {
		agenthostLog.Warn("relaying a commit signature request failed; answering it unanswered",
			"conversation", r.info.ConversationID, "error", err)
		return unanswered
	}
	res = commitsigner.Result{
		Outcome:           commitsigner.Outcome(reply.Outcome),
		SignatureID:       reply.SignatureID,
		Signature:         reply.Signature,
		Refusal:           commitsigner.Refusal(reply.Reason),
		RetryAfterSeconds: reply.RetryAfterSeconds,
		Limit:             reply.Limit,
		SignerStatus:      domain.CommitSignerStatus(reply.SignerStatus),
		SignerReason:      reply.SignerStatusReason,
	}
	if res.Outcome != commitsigner.OutcomeSigned {
		return res
	}
	if res.Signature == "" {
		agenthostLog.Error("the orchestrator answered a signature request signed with no signature; answering it unanswered",
			"conversation", r.info.ConversationID, "signature", res.SignatureID)
		return unanswered
	}
	// The commit id git will give the commit, recorded on the signature's row
	// so a push of it can name its signer. Sent through the same record lane
	// as the git proxy's push records, so it reaches the orchestrator before
	// any push of the commit does.
	res.CommitSHA = commitsign.CommitID(commitsign.SignedCommit(payload, []byte(res.Signature)))
	r.conn.record(agentproc.RelayNamespaceCore, agentproc.OpRecordCommitSHA, agentproc.RecordCommitSHAArgs{
		SignatureID: res.SignatureID,
		CommitSHA:   res.CommitSHA,
	})
	return res
}

// recordCommitSHA stores a relayed commit id on the signature's row. The row
// must be this run's: the wire names it, and a sidecar may stamp only its own
// run's signatures. A row is given its commit id once, so a resent record
// writes nothing new.
func (s *RelayServer) recordCommitSHA(ctx context.Context, a agentproc.RecordCommitSHAArgs) error {
	if uuid.Validate(a.SignatureID) != nil || !isCommitID(a.CommitSHA) {
		return fmt.Errorf("agenthost: a relayed commit id names no signature or no commit (%q, %q)", a.SignatureID, a.CommitSHA)
	}
	if s.stores.CommitSignatures == nil {
		return errors.New("agenthost: no signature store to record a commit id in")
	}
	ctx, cancel := recordContext(ctx)
	defer cancel()
	var row *domain.CommitSignature
	if err := withRecordRetry(ctx, func() error {
		var e error
		row, e = s.stores.CommitSignatures.GetSystem(ctx, s.info.OrgID, a.SignatureID)
		return e
	}); err != nil {
		recordWriteDrop(ctx, agentproc.OpRecordCommitSHA, err)
		return nil
	}
	if row == nil || row.ConversationID != s.info.ConversationID {
		return fmt.Errorf("agenthost: a relayed commit id names signature %s, which is not this run's", a.SignatureID)
	}
	if err := withRecordRetry(ctx, func() error {
		_, e := s.stores.CommitSignatures.SetCommitSHASystem(ctx, s.info.OrgID, a.SignatureID, a.CommitSHA)
		return e
	}); err != nil {
		recordWriteDrop(ctx, agentproc.OpRecordCommitSHA, err)
	}
	return nil
}

// recordSignRefusal records a commit the sidecar's daemon refused. The wire
// says only which refusal and the committer. The org and conversation are this
// server's, the signer's host the org's, and the signer the one the sidecar
// reported at bring-up (SetSealedCommitSigner), before the agent ran: the
// account an audit row names is never one a sidecar chose mid-run.
func (s *RelayServer) recordSignRefusal(ctx context.Context, a agentproc.RecordSignRefusalArgs) error {
	refusal := commitsigner.Refusal(a.Reason)
	if refusal != commitsigner.RefusalCommitterMismatch && refusal != commitsigner.RefusalNotACommit {
		return fmt.Errorf("agenthost: a relayed signing refusal names no daemon refusal (%q)", a.Reason)
	}
	accountID, err := strconv.ParseInt(s.sealed.accountID, 10, 64)
	if !s.sealed.mode.Signs() || err != nil || accountID <= 0 {
		return fmt.Errorf("agenthost: a signing refusal from a run that was sealed no signer (%q, account %q)",
			s.sealed.mode, s.sealed.accountID)
	}
	ctx, cancel := recordContext(ctx)
	defer cancel()
	host, err := s.signerHost(ctx)
	if err != nil {
		return fmt.Errorf("agenthost: record a signing refusal: %w", err)
	}
	commitsigner.RecordDaemonRefusal(ctx, s.stores, commitsigner.DaemonRefusal{
		OrgID:          s.info.OrgID,
		ConversationID: s.info.ConversationID,
		Signer:         s.sealed.mode,
		SignerHost:     host,
		SignerUserID:   accountID,
		Refusal:        refusal,
		Committer:      commitsign.Identity{Name: a.CommitterName, Email: a.CommitterEmail},
		DedupKey:       mintDedupKey(ctx, s.info),
	})
	return nil
}

// isCommitID reports whether s is a commit id as git spells one: 40 lowercase
// hex digits.
func isCommitID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
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
// sealed is the signer the run's bundle named, on an executor
// (countSigningPush).
func pushSigner(ctx context.Context, stores db.Stores, orgID, sha string, sealed domain.CommitSigningMode) string {
	if stores.CommitSignatures == nil || sha == "" {
		return ""
	}
	signer, signed, err := commitsigner.TipSigner(ctx, stores, orgID, sha)
	if err != nil {
		agenthostLog.Warn("reading the signature of a pushed commit failed; its audit row names no signer",
			"org", orgID, "sha", sha, "error", err)
		return ""
	}
	countSigningPush(ctx, stores, orgID, sealed, signed)
	return signer
}

// MarkPushedSignature stamps the signature that produced a landed push's new
// tip with the repository and the push time, when one of the org's
// signatures produced it, so the control plane's post-push check reads the
// commit's verification on its next GitHub cycle. The first push of a commit
// is the one kept. Host-side only, like RecordGitPushFailed: the git proxy's
// push observer calls it for a branch the upstream accepted. The repository is
// the registry row GitHub's id for it names (githubRepositoryRow), as for the
// push's own rows. Best-effort: the push already happened, so a failure is
// logged and the commit goes unchecked.
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
	host, err := db.OrgGitHubHostSystem(ctx, c.stores.Orgs, c.info.OrgID)
	var row *domain.Repository
	if err == nil {
		row, err = githubRepositoryRow(ctx, c.stores, c.info, host, PushIdentity(push))
	}
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
// run's org resolves to a signer that signs. A multi-mode push is recorded on
// the executor, which cannot read the org's signer, so there it counts when
// the run's sealed bundle named one (sealed); locally the org is resolved.
func countSigningPush(ctx context.Context, stores db.Stores, orgID string, sealed domain.CommitSigningMode, signed bool) {
	if runmode.Current() == runmode.ModeMulti {
		if sealed.Signs() {
			commitsigner.CountPush(ctx, orgID, signed)
		}
		return
	}
	if stores.OrgCommitSigners == nil {
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
