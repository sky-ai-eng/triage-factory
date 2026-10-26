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
	"github.com/sky-ai-eng/triage-factory/internal/ctlbus"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
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

	// decideTimeout bounds one request's claim, decision and write.
	decideTimeout = 10 * time.Second

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
// The budgets are counted before signing, from rows, so two pods signing for
// one org at the same instant can each admit one past the limit. The limits
// bound a runaway or compromised run, which that does not change.
type ControlPlane struct {
	stores  db.Stores
	budgets Budgets
	podID   string
	publish Publisher
	now     func() time.Time
	nudges  chan requestRef
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
// A decision that cannot be stored leaves the row signing, and the run that
// asked stops waiting at its deadline.
func (c *ControlPlane) Handle(ctx context.Context, orgID, id string) bool {
	ctx, cancel := context.WithTimeout(ctx, decideTimeout)
	defer cancel()
	row, err := c.stores.CommitSignatures.ClaimRequestSystem(ctx, orgID, id, c.podID)
	if err != nil {
		signerLog.Warn("claiming a signature request failed", "org", orgID, "signature", id, "error", err)
		return false
	}
	if row == nil {
		return false
	}

	ctx, span := tracer.Start(ctx, "commit_signing.decide", trace.WithAttributes(
		telemetry.OrgID(row.OrgID),
		telemetry.ConversationID(row.ConversationID),
		telemetry.Signer(string(row.Signer)),
	))
	defer span.End()

	d := c.decide(ctx, *row)
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
// private half, or refuse it. denied names the commit_sign_denied row a
// refusal records, "" for none.
type decision struct {
	refusal    Refusal
	retryAfter int
	denied     string
	publicKey  string
	signature  []byte
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

	signer, refusal, err := c.checkSigner(ctx, row)
	if err != nil {
		return c.undecided(ctx, row, "resolve the org's signer", err)
	}
	if refusal != "" {
		return decision{refusal: refusal}
	}

	claimed, err := c.stores.CommitSignatures.ClaimSignaturesSystem(ctx, row.OrgID, row.ClaimID)
	if err != nil {
		return c.undecided(ctx, row, "count the claim's signatures", err)
	}
	if claimed >= c.budgets.PerClaim {
		signerLog.WarnContext(ctx, "refused a signature: the engagement spent its signing budget",
			"org", row.OrgID, "conversation", row.ConversationID, "claim", row.ClaimID, "limit", c.budgets.PerClaim)
		return decision{refusal: RefusalClaimBudget, denied: deniedBudgetExhausted}
	}
	window, err := c.stores.CommitSignatures.OrgSignaturesSinceSystem(ctx, row.OrgID, now.Add(-OrgWindow))
	if err != nil {
		return c.undecided(ctx, row, "count the org's signatures", err)
	}
	if window.Count >= c.budgets.PerOrgMinute {
		return decision{refusal: RefusalOrgMinuteBudget, retryAfter: retryAfter(window.Oldest, now)}
	}

	key, err := c.signingKey(ctx, row.OrgID, signer.PublicKey)
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

// checkSigner resolves the org's signer now and compares it with the one the
// request names: the same kind, the same account on the same host, and
// active. It returns the resolved identity when they match, and the refusal
// when they do not.
func (c *ControlPlane) checkSigner(ctx context.Context, row domain.CommitSignature) (Identity, Refusal, error) {
	if row.Signer == domain.CommitSigningDeployment {
		// TODO(TFAC-1200): sign as the deployment signer, with the key from
		// the control pods' config, while it is active and the org resolves
		// to it. Until then no deployment signer exists to sign as.
		return Identity{}, RefusalSignerInactive, nil
	}
	id, err := Resolver{Signers: c.stores.OrgCommitSigners}.Resolve(ctx, row.OrgID)
	if err != nil {
		return Identity{}, "", err
	}
	switch {
	case id.AccountID == "":
		return Identity{}, RefusalSignerRemoved, nil
	case id.AccountID != strconv.FormatInt(row.SignerUserID, 10):
		return Identity{}, RefusalSignerChanged, nil
	case !id.Mode.Signs():
		return Identity{}, RefusalSignerInactive, nil
	case id.Mode != row.Signer || db.NormalizeGitHubHost(id.Host) != row.SignerHost:
		return Identity{}, RefusalSignerChanged, nil
	}
	return id, "", nil
}

// signingKey returns the private key whose public half is publicKey, from the
// org's signing-key secret or, while a verify promotes a rotation, its pending
// twin: the two are swapped beside the signer row, so for a moment the key the
// row names can be in either.
func (c *ControlPlane) signingKey(ctx context.Context, orgID, publicKey string) (ed25519.PrivateKey, error) {
	for _, name := range []string{integrations.KeyGitHubCommitSigningKey, integrations.KeyGitHubCommitSigningKeyPending} {
		raw, err := c.stores.Secrets.GetSystem(ctx, orgID, name)
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
		_, err := c.stores.CommitSignatures.RefuseSystem(ctx, row.OrgID, row.ID, c.podID, string(d.refusal), d.retryAfter)
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

// recordDenied writes the commit_sign_denied audit row for a refusal that is a
// security signal. Nothing was signed, so it is written under CredentialNone,
// about the signer's account on its host. Best-effort, like every audit row a
// gate writes: the refusal is the row this pod already stored, and does not
// depend on this one.
func (c *ControlPlane) recordDenied(ctx context.Context, row domain.CommitSignature, reason string) {
	if c.stores.ExternalActions == nil {
		return
	}
	detail, _ := json.Marshal(map[string]string{"reason": reason})
	act := domain.ExternalAction{
		Provider:       domain.ArtifactProviderGitHub,
		Scope:          domain.GitHubHost(row.SignerHost),
		Action:         domain.ActionCommitSignDenied,
		Target:         c.signerLogin(ctx, row),
		ExternalID:     strconv.FormatInt(row.SignerUserID, 10),
		ConversationID: row.ConversationID,
		Credential:     domain.CredentialNone,
		DetailJSON:     string(detail),
	}
	if row.ConversationID != "" {
		if reason == deniedBudgetExhausted {
			act.DedupKey = domain.CommitSignBudgetDedupKey(row.ConversationID)
		}
		conv, err := c.stores.Conversations.GetSystem(ctx, row.OrgID, row.ConversationID)
		if err != nil {
			signerLog.WarnContext(ctx, "reading the conversation for a commit_sign_denied row failed; recording it without its team",
				"org", row.OrgID, "conversation", row.ConversationID, "error", err)
		} else if conv != nil {
			act.TeamID, act.ActorUserID = conv.TeamID, conv.CreatorUserID
		}
	}
	if err := c.stores.ExternalActions.RecordSystem(ctx, row.OrgID, act); err != nil {
		signerLog.WarnContext(ctx, "recording a commit_sign_denied row failed",
			"org", row.OrgID, "conversation", row.ConversationID, "reason", reason, "error", err)
	}
}

// signerLogin names the request's signer for the audit row: the login of the
// org's signer when it is still the account the request names, else the
// account's id.
func (c *ControlPlane) signerLogin(ctx context.Context, row domain.CommitSignature) string {
	if signer, err := c.stores.OrgCommitSigners.GetSystem(ctx, row.OrgID); err == nil && signer != nil &&
		signer.GitHubUserID == row.SignerUserID {
		return signer.Login
	}
	return strconv.FormatInt(row.SignerUserID, 10)
}
