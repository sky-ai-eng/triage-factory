package commitsigner

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"sync"
	"time"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/sky-ai-eng/triage-factory/internal/commitsign"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/pgnotify/ctlbus"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
)

const (
	// SweepInterval is how often every control pod looks for requests whose
	// sign_request doorbell it never heard. tf_ctl is lossy, so this is what
	// bounds a dropped doorbell's cost.
	SweepInterval = time.Second

	// sweepMinAge keeps the sweep off requests younger than this, whose
	// doorbell is most likely still on its way: a request the sweep takes is
	// counted as one tf_ctl dropped.
	sweepMinAge = 500 * time.Millisecond

	// sweepBatch caps one sweep. More than this many requests waiting means
	// every doorbell is being dropped, and the next sweep a second later
	// takes the rest.
	sweepBatch = 100

	// signerWorkers is how many requests one pod decides at once from its
	// doorbells. A decision is a few row reads and a signature, so a handful
	// keeps one slow read from holding the others back.
	signerWorkers = 4

	// nudgeQueueDepth bounds the doorbells a pod holds undecided. A ring that
	// finds it full is dropped, like any other lost doorbell, and the sweep
	// finds its request.
	nudgeQueueDepth = 256

	// decideTimeout bounds one request's claim and decision.
	decideTimeout = 10 * time.Second

	// writeTimeout bounds storing a decision, ringing the run and recording
	// a refusal's audit row.
	writeTimeout = 5 * time.Second

	// expiryMargin is how far past RequestTimeout a request must be before a
	// pod closes it unanswered rather than signing it. The run stopped
	// waiting at RequestTimeout by its own clock; the margin absorbs the
	// difference between that clock and this pod's.
	expiryMargin = 10 * time.Second
)

// errNoSigningKey is a signer whose private key the secret store does not
// hold. Briefly true while a rotation swaps its keys, so it is answered
// unanswered, which the run retries.
var errNoSigningKey = errors.New("commitsigner: the secret store holds no private key for the signer's public key")

// errNoDeploymentSigner is a deployment request decided by a control pod
// configured with no deployment signer, or with a key other than the one the
// org resolves to. Every control pod should carry the same signer config; a
// pod that does not answers unanswered, and the run's next attempt can land on
// one that does.
var errNoDeploymentSigner = errors.New("commitsigner: this control pod is not configured with the deployment signer the request names")

// ControlPlane is a control pod's half of a multi-mode signature. It claims a
// queued request, checks that the engagement that asked still holds its claim,
// that the org still signs as the signer the request names, and that the claim
// and the org are within their budgets, then signs the digest with the
// signer's key from the secret store or refuses, and rings the executor
// waiting on the answer.
//
// Every control pod runs one, whether or not it holds the background-brain
// lease: a signature waits on whichever pod answers first, and a lease
// handover would otherwise stall every commit for its length. Pods race for
// each request through ClaimRequestSystem's conditional update, so each is
// decided once.
//
// The budgets are counted before signing, from rows, so every request for one
// org decided at the same instant, by any pod's workers or sweep, can admit one
// past the limit: at most signerWorkers+1 per control pod. The limits bound a
// runaway or compromised run, which that does not change.
//
// A request as the deployment signer is decided the same way, with the key
// from this pod's config (SetDeploymentSigner) instead of the secret store.
type ControlPlane struct {
	stores     db.Stores
	budgets    Budgets
	podID      string
	publish    Publisher
	deployment *DeploymentSigner
	now        func() time.Time
	nudges     chan requestRef
}

type requestRef struct{ orgID, id string }

// NewControlPlane returns a control pod's signer. podID names the pod on the
// rows it claims; publish rings tf_ctl.
func NewControlPlane(stores db.Stores, budgets Budgets, podID string, publish Publisher) *ControlPlane {
	return &ControlPlane{
		stores:  stores,
		budgets: budgets,
		podID:   podID,
		publish: publish,
		now:     time.Now,
		nudges:  make(chan requestRef, nudgeQueueDepth),
	}
}

// SetDeploymentSigner gives the pod the deployment's signer, which it signs
// deployment requests with while every org on the deployment App with no
// signer of its own resolves to it. nil, the default, signs none. Call it
// before Run.
func (c *ControlPlane) SetDeploymentSigner(d *DeploymentSigner) {
	c.deployment = d
}

// resolver is the identity resolution every decision makes again, with the
// deployment arm when this pod has a deployment signer.
func (c *ControlPlane) resolver() Resolver {
	r := Resolver{Signers: c.stores.OrgCommitSigners}
	if c.deployment.Configured() {
		r.Deployment = &DeploymentArm{Signer: c.deployment, Health: c.stores.DeploymentCommitSigners, Orgs: c.stores.Orgs}
	}
	return r
}

// Nudge is the sign_request doorbell: a request was queued. It hands the
// request to the pod's workers without blocking, because the tf_ctl read loop
// calls it, and drops it when they are behind; the sweep finds it then. A nil
// signer (a process that signs nothing) ignores it.
func (c *ControlPlane) Nudge(orgID, id string) {
	if c == nil || id == "" {
		return
	}
	select {
	case c.nudges <- requestRef{orgID: orgID, id: id}:
	default:
	}
}

// Run decides the requests its doorbells name and sweeps once a second for the
// rest, until ctx is cancelled.
func (c *ControlPlane) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for range signerWorkers {
		wg.Go(func() {
			for {
				select {
				case <-ctx.Done():
					return
				case ref := <-c.nudges:
					c.Handle(ctx, ref.orgID, ref.id)
				}
			}
		})
	}
	ticker := time.NewTicker(SweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case <-ticker.C:
			c.Sweep(ctx)
		}
	}
}

// Sweep decides the requests no pod has claimed, oldest first, and counts each
// one this pod took as a doorbell tf_ctl dropped.
func (c *ControlPlane) Sweep(ctx context.Context) {
	rows, err := c.stores.CommitSignatures.ListRequestedSystem(ctx, sweepMinAge, sweepBatch)
	if err != nil {
		signerLog.Warn("sweeping for signature requests failed", "error", err)
		return
	}
	for _, row := range rows {
		if c.Handle(ctx, row.OrgID, row.ID) {
			current().swept.Add(ctx, 1)
		}
	}
}

// Handle claims one request and, when this pod got it, decides it, stores the
// signature or the refusal, and rings sign_done. It reports whether this pod
// claimed the request; false is another pod having it, or a request that is
// not one.
//
// The decision is written on a budget of its own, detached from ctx and from
// the one the claim and decision spent: a decision that ran out of time, or
// whose pod began shutting down while it decided, is still stored, as
// unanswered when its reads failed. A decision that cannot be stored even then
// leaves the row signing, and the run that asked stops waiting at its
// deadline.
func (c *ControlPlane) Handle(ctx context.Context, orgID, id string) bool {
	decideCtx, cancel := context.WithTimeout(ctx, decideTimeout)
	defer cancel()
	row, err := c.stores.CommitSignatures.ClaimRequestSystem(decideCtx, orgID, id, c.podID)
	if err != nil {
		signerLog.Warn("claiming a signature request failed", "org", orgID, "signature", id, "error", err)
		return false
	}
	if row == nil {
		return false
	}

	decideCtx, span := tracer.Start(decideCtx, "commit_signing.decide", trace.WithAttributes(
		telemetry.OrgID(row.OrgID),
		telemetry.ConversationID(row.ConversationID),
		telemetry.Signer(string(row.Signer)),
	))
	defer span.End()

	d := c.decide(decideCtx, *row)

	ctx, cancelWrite := context.WithTimeout(context.WithoutCancel(decideCtx), writeTimeout)
	defer cancelWrite()
	if err := c.store(ctx, *row, d); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		signerLog.ErrorContext(ctx, "storing a signature decision failed; the run will stop waiting at its deadline",
			"org", row.OrgID, "signature", row.ID, "error", err)
		return true
	}
	if d.refusal == "" {
		span.SetAttributes(telemetry.Outcome(string(OutcomeSigned)))
	} else {
		span.SetAttributes(telemetry.Outcome(string(OutcomeRefused)), telemetry.Reason(string(d.refusal)))
	}
	if err := c.publish(ctx, ctlbus.Message{Kind: KindSignDone, OrgID: row.OrgID, SignatureID: row.ID}); err != nil {
		// The waiting run reads the row within its poll interval.
		signerLog.WarnContext(ctx, "ringing the run for a decided signature request failed",
			"org", row.OrgID, "signature", row.ID, "error", err)
	}
	if d.denied != "" {
		c.recordDenied(ctx, *row, d.denied)
	}
	return true
}

// decision is what a pod does with one request: sign it with publicKey's
// private half, or refuse it. limit is the budget a budget refusal ran into,
// and signerStatus and signerReason the signer's status and its reason for a
// refusal because the signer is not active: the run's refusal line names
// them. denied names the commit_sign_denied row a refusal records, "" for
// none.
type decision struct {
	refusal      Refusal
	retryAfter   int
	limit        int
	signerStatus domain.CommitSignerStatus
	signerReason string
	denied       string
	publicKey    string
	signature    []byte
}

// decide runs a claimed request's checks in order and signs when they pass.
// A read it cannot make leaves the request unanswered rather than signed: it
// never signs what it could not check.
func (c *ControlPlane) decide(ctx context.Context, row domain.CommitSignature) decision {
	now := c.now()
	if now.Sub(row.CreatedAt) > RequestTimeout+expiryMargin {
		return decision{refusal: RefusalUnanswered}
	}

	live, err := c.stores.ConversationQueue.ClaimLiveSystem(ctx, row.OrgID, row.ClaimID, row.BootEpoch)
	if err != nil {
		return c.undecided(ctx, row, "read the claim", err)
	}
	if !live {
		signerLog.WarnContext(ctx, "refused a signature for an engagement that no longer holds its claim",
			"org", row.OrgID, "conversation", row.ConversationID, "claim", row.ClaimID, "boot_epoch", row.BootEpoch)
		return decision{refusal: RefusalClaimInactive, denied: deniedClaimInactive}
	}

	if row.Signer == domain.CommitSigningDeployment && !c.deployment.Configured() {
		return c.undecided(ctx, row, "find the deployment signer", errNoDeploymentSigner)
	}
	signer, err := c.resolver().Resolve(ctx, row.OrgID)
	if err != nil {
		return c.undecided(ctx, row, "resolve the org's signer", err)
	}
	// The run's line says why signing stopped for a signer that is still the
	// org's but not active, from the status the resolution read.
	switch refusal := signerRefusal(signer, row.Signer, row.SignerHost, strconv.FormatInt(row.SignerUserID, 10)); refusal {
	case "":
	case RefusalSignerInactive:
		return decision{refusal: refusal, signerStatus: signer.Status, signerReason: signer.StatusReason}
	default:
		return decision{refusal: refusal}
	}

	if res, ok, err := checkBudgets(ctx, c.stores.CommitSignatures, c.budgets, row.OrgID, row.ClaimID, now); err != nil {
		return c.undecided(ctx, row, "count the signatures", err)
	} else if !ok {
		if res.Refusal == RefusalClaimBudget {
			signerLog.WarnContext(ctx, "refused a signature: the engagement spent its signing budget",
				"org", row.OrgID, "conversation", row.ConversationID, "claim", row.ClaimID, "limit", res.Limit)
			return decision{refusal: RefusalClaimBudget, limit: res.Limit, denied: deniedBudgetExhausted}
		}
		return decision{refusal: res.Refusal, retryAfter: res.RetryAfterSeconds, limit: res.Limit}
	}

	key, err := c.signingKey(ctx, row.OrgID, signer)
	if err != nil {
		return c.undecided(ctx, row, "read the signer's key", err)
	}
	digest, err := commitsign.ParseDigest(row.Digest)
	if err != nil {
		return c.undecided(ctx, row, "read the request's digest", err)
	}
	sig, err := commitsign.SignDigest(key, digest)
	if err != nil {
		return c.undecided(ctx, row, "sign the digest", err)
	}
	return decision{publicKey: signer.PublicKey, signature: sig}
}

// undecided logs why a request could not be decided and closes it unanswered,
// which the run answers by running the same command again.
func (c *ControlPlane) undecided(ctx context.Context, row domain.CommitSignature, step string, err error) decision {
	signerLog.ErrorContext(ctx, "could not decide a signature request; closing it unanswered",
		"step", step, "org", row.OrgID, "signature", row.ID, "error", err)
	return decision{refusal: RefusalUnanswered}
}

// signingKey returns the private key that signs as signer: the deployment
// signer's from this pod's config, or the org's from the secret store.
func (c *ControlPlane) signingKey(ctx context.Context, orgID string, signer Identity) (ed25519.PrivateKey, error) {
	if signer.Mode == domain.CommitSigningDeployment {
		if !c.deployment.Configured() || c.deployment.PublicKey != signer.PublicKey {
			return nil, errNoDeploymentSigner
		}
		return c.deployment.key, nil
	}
	return signingKey(ctx, c.stores.Secrets, orgID, signer.PublicKey)
}

// signerRefusal compares the signer an org resolves to now with the one a
// signature is asked for as: mode, an account id in text, and the host that
// account is on. It returns the refusal when they are not the same kind of
// signer, on the same account on the same host, and active, and "" when the
// signature may be made. The refusal says which of those failed, because the
// run turns it into the sentence its agent reads.
func signerRefusal(now Identity, mode domain.CommitSigningMode, host, accountID string) Refusal {
	switch {
	case now.AccountID == "":
		return RefusalSignerRemoved
	case now.AccountID != accountID:
		return RefusalSignerChanged
	case !now.Mode.Signs():
		return RefusalSignerInactive
	case now.Mode != mode || db.NormalizeGitHubHost(now.Host) != db.NormalizeGitHubHost(host):
		return RefusalSignerChanged
	}
	return ""
}

// signingKey returns the private key whose public half is publicKey, from the
// org's signing-key secret or, while a verify promotes a rotation, its pending
// twin: the two are swapped beside the signer row, so for a moment the key the
// row names can be in either.
func signingKey(ctx context.Context, secrets db.SecretStore, orgID, publicKey string) (ed25519.PrivateKey, error) {
	for _, name := range []string{integrations.KeyGitHubCommitSigningKey, integrations.KeyGitHubCommitSigningKeyPending} {
		raw, err := secrets.GetSystem(ctx, orgID, name)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		if raw == "" {
			continue
		}
		key, err := commitsign.ParsePrivateKey([]byte(raw))
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
		pub, err := commitsign.PublicKey(key)
		if err != nil {
			return nil, fmt.Errorf("derive %s's public key: %w", name, err)
		}
		if pub == publicKey {
			return key, nil
		}
	}
	return nil, errNoSigningKey
}

// store writes the decision onto the row this pod claimed.
func (c *ControlPlane) store(ctx context.Context, row domain.CommitSignature, d decision) error {
	if d.refusal != "" {
		_, err := c.stores.CommitSignatures.RefuseSystem(ctx, row.OrgID, row.ID, c.podID, db.CommitSignatureRefusal{
			Reason:             string(d.refusal),
			RetryAfterSeconds:  d.retryAfter,
			Limit:              d.limit,
			SignerStatus:       d.signerStatus,
			SignerStatusReason: d.signerReason,
		})
		return err
	}
	_, err := c.stores.CommitSignatures.SignSystem(ctx, row.OrgID, row.ID, c.podID, d.publicKey, string(d.signature), c.now())
	return err
}

// retryAfter is how many whole seconds until oldest leaves the org's budget
// window, so the budget admits another signature: at least one.
func retryAfter(oldest *time.Time, now time.Time) int {
	if oldest == nil {
		return 1
	}
	wait := oldest.Add(OrgWindow).Sub(now)
	return max(int(math.Ceil(wait.Seconds())), 1)
}

// recordDenied writes the commit_sign_denied audit row for a refusal of row
// that is a security signal.
func (c *ControlPlane) recordDenied(ctx context.Context, row domain.CommitSignature, reason string) {
	recordDenied(ctx, c.stores, denial{
		orgID:          row.OrgID,
		conversationID: row.ConversationID,
		signerHost:     row.SignerHost,
		signerUserID:   row.SignerUserID,
		reason:         reason,
	})
}

// denial is one commit_sign_denied audit row: the refusal's reason, the
// account the signature was asked for as, and the run that asked. committer is
// set for a committer mismatch, as the committer the refused commit named.
// dedupKey keys a row that may be recorded more than once, "" for one that
// is recorded once; a budget refusal is keyed per conversation whatever it
// says.
type denial struct {
	orgID          string
	conversationID string
	signerHost     string
	signerUserID   int64
	reason         string
	committer      string
	dedupKey       string
}

// recordDenied writes the commit_sign_denied audit row for a refusal that is a
// security signal. Nothing was signed, so it is written under CredentialNone,
// about the signer's account on its host, and performed by the agent, whose
// attempt it refuses. Best-effort, like every audit row a
// gate writes: the refusal is already decided, and does not depend on this
// row.
func recordDenied(ctx context.Context, stores db.Stores, d denial) {
	if stores.ExternalActions == nil {
		return
	}
	detail := map[string]string{"reason": d.reason}
	if d.committer != "" {
		detail["committer"] = d.committer
	}
	raw, _ := json.Marshal(detail)
	act := domain.ExternalAction{
		Provider:       domain.ProviderGitHub,
		Scope:          domain.GitHubHost(d.signerHost),
		Action:         domain.ActionCommitSignDenied,
		Target:         signerLogin(ctx, stores.OrgCommitSigners, d.orgID, d.signerUserID),
		ExternalID:     strconv.FormatInt(d.signerUserID, 10),
		ConversationID: d.conversationID,
		Credential:     domain.CredentialNone,
		DetailJSON:     string(raw),
		DedupKey:       d.dedupKey,
		PerformedBy:    domain.PerformedByAgent,
	}
	if d.conversationID != "" {
		if d.reason == deniedBudgetExhausted {
			act.DedupKey = domain.CommitSignBudgetDedupKey(d.conversationID)
		}
		conv, err := stores.Conversations.GetSystem(ctx, d.orgID, d.conversationID)
		if err != nil {
			signerLog.WarnContext(ctx, "reading the conversation for a commit_sign_denied row failed; recording it without its team",
				"org", d.orgID, "conversation", d.conversationID, "error", err)
		} else if conv != nil {
			act.TeamID, act.ActorUserID = conv.TeamID, conv.CreatorUserID
		}
	}
	if err := stores.ExternalActions.RecordSystem(ctx, d.orgID, act); err != nil {
		signerLog.WarnContext(ctx, "recording a commit_sign_denied row failed",
			"org", d.orgID, "conversation", d.conversationID, "reason", d.reason, "error", err)
	}
}

// signerLogin names the account userID for an audit row: the login of the
// org's signer when it is still that account, else the account's id.
func signerLogin(ctx context.Context, signers db.OrgCommitSignerStore, orgID string, userID int64) string {
	if signers != nil {
		if signer, err := signers.GetSystem(ctx, orgID); err == nil && signer != nil && signer.GitHubUserID == userID {
			return signer.Login
		}
	}
	return strconv.FormatInt(userID, 10)
}
