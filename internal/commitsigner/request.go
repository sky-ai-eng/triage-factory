package commitsigner

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/sky-ai-eng/triage-factory/internal/commitsign"
	"github.com/sky-ai-eng/triage-factory/internal/ctlbus"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
)

var tracer = otel.Tracer("internal/commitsigner")

const (
	// RequestTimeout is how long a run waits for a control pod to decide its
	// signature request. Past it the run is told nothing answered, and to
	// run the same git command again.
	RequestTimeout = 30 * time.Second

	// requestPollInterval is how often a waiting request reads its row, the
	// backstop for a sign_done that tf_ctl dropped.
	requestPollInterval = 250 * time.Millisecond
)

// Publisher sends one tf_ctl message. ctlbus.Publish over the admin pool, in
// production.
type Publisher func(ctx context.Context, msg ctlbus.Message) error

// Requester is the executor's half of a multi-mode signature: it queues a
// sandboxed run's digest for the control plane and waits for the answer. The
// executor holds no signing key, so it never signs; a control pod does
// (ControlPlane).
type Requester struct {
	Signatures db.CommitSignatureStore
	Publish    Publisher
	Doorbells  *Doorbells

	// Timeout and PollInterval replace RequestTimeout and the row-read
	// interval when set. Tests shorten them.
	Timeout      time.Duration
	PollInterval time.Duration
}

// Request is one signature a run asks for. Every field but Signer,
// SignerUserID and Digest is bound by the executor from the run it serves,
// never taken from the sandbox: the org and conversation from the run's
// identity, the claim and boot epoch from the engagement, the host from the
// org's settings.
type Request struct {
	OrgID          string
	ConversationID string
	ClaimID        string
	BootEpoch      int64

	// Signer and SignerUserID are the signer the run was sealed with. A
	// control pod signs only when the org still resolves to exactly that
	// signer, so a sandbox that names another one gains nothing.
	Signer       domain.CommitSigningMode
	SignerHost   string
	SignerUserID int64

	Digest commitsign.Digest
}

// Result is a request's answer. SignatureID names its commit_signatures row,
// which the run's commit id is recorded on once the run has computed it.
type Result struct {
	Outcome     Outcome
	SignatureID string

	// Signature is the armored SSH signature, and PublicKey the key that made
	// it, for OutcomeSigned.
	Signature string
	PublicKey string

	// Refusal says why, for OutcomeRefused; RetryAfterSeconds is how long
	// until the org's per-minute budget admits another, set only for
	// RefusalOrgMinuteBudget.
	Refusal           Refusal
	RetryAfterSeconds int

	// The rest fill in the line the run's agent reads (RefusalLine) and are
	// set only by a signer that knows them.
	//
	// CommitSHA is the id git gives the signed commit, for OutcomeSigned.
	CommitSHA string
	// Limit is the budget a RefusalOrgMinuteBudget or RefusalClaimBudget
	// refusal ran into.
	Limit int
	// Committer is the committer a RefusalCommitterMismatch commit named.
	Committer commitsign.Identity
	// Object is what a RefusalNotACommit payload was: commitsign.ObjectTag,
	// commitsign.ObjectPushCertificate, or "" for no object git signs.
	Object string
	// SignerStatus and SignerReason are the signer's status and its reason,
	// for a RefusalSignerInactive of an org signer.
	SignerStatus domain.CommitSignerStatus
	SignerReason string
}

// errRequestVanished is a waiting request whose row is gone: its org was
// deleted under it.
var errRequestVanished = errors.New("commitsigner: the signature request's row is gone")

// Request queues req for the control plane and returns its answer: the
// signature, a refusal, or, once the timeout passes with neither,
// OutcomeUnanswered. An error is a request that could not be queued or waited
// on at all.
func (r *Requester) Request(ctx context.Context, req Request) (res Result, err error) {
	start := time.Now()
	ctx, span := tracer.Start(ctx, "commit_signing.sign", trace.WithAttributes(
		telemetry.OrgID(req.OrgID),
		telemetry.ConversationID(req.ConversationID),
		telemetry.Signer(string(req.Signer)),
		telemetry.SigningMode(modeControlPlane),
	))
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		} else {
			span.SetAttributes(telemetry.Outcome(string(res.Outcome)))
			if res.Refusal != "" {
				span.SetAttributes(telemetry.Reason(string(res.Refusal)))
			}
			countRequest(req.Signer, req.OrgID, res.Outcome, res.Refusal)
			if res.Outcome == OutcomeSigned {
				observeDuration(req.Signer, modeControlPlane, time.Since(start))
			}
		}
		span.End()
	}()

	row, err := r.Signatures.InsertSystem(ctx, domain.CommitSignature{
		OrgID:          req.OrgID,
		ConversationID: req.ConversationID,
		ClaimID:        req.ClaimID,
		BootEpoch:      req.BootEpoch,
		Signer:         req.Signer,
		SignerHost:     req.SignerHost,
		SignerUserID:   req.SignerUserID,
		Digest:         req.Digest.String(),
		State:          domain.CommitSignatureRequested,
	})
	if err != nil {
		return Result{}, fmt.Errorf("queue the signature request: %w", err)
	}

	// Listen before ringing, so a pod that decides before this goroutine is
	// back from the publish still finds a listener.
	ring, stop := r.Doorbells.Listen(row.ID)
	defer stop()
	if err := r.Publish(ctx, ctlbus.Message{Kind: KindSignRequest, OrgID: req.OrgID, SignatureID: row.ID}); err != nil {
		// The sweep finds the request within a second.
		signerLog.WarnContext(ctx, "ringing the control plane for a signature request failed; the sweep will find it",
			"org", req.OrgID, "signature", row.ID, "error", err)
	}
	return r.await(ctx, req.OrgID, row.ID, ring)
}

// await reads the request's row until a control pod has decided it, waking on
// its doorbell or the poll, and gives up at the timeout.
func (r *Requester) await(ctx context.Context, orgID, id string, ring <-chan struct{}) (Result, error) {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = RequestTimeout
	}
	poll := r.PollInterval
	if poll <= 0 {
		poll = requestPollInterval
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	// The poll is fast enough that a line per failed read would bury the
	// signal; one names the condition and the outcome reports the rest.
	warned := false
	for {
		row, err := r.Signatures.GetSystem(ctx, orgID, id)
		switch {
		case err != nil:
			if !warned {
				warned = true
				signerLog.WarnContext(ctx, "reading a signature request failed; retrying until its deadline",
					"org", orgID, "signature", id, "error", err)
			}
		case row == nil:
			return Result{}, errRequestVanished
		case row.State == domain.CommitSignatureSigned:
			return Result{Outcome: OutcomeSigned, SignatureID: id, Signature: row.Signature, PublicKey: row.PublicKey}, nil
		case row.State == domain.CommitSignatureRefused && Refusal(row.RefusalReason) == RefusalUnanswered:
			return Result{Outcome: OutcomeUnanswered, SignatureID: id}, nil
		case row.State == domain.CommitSignatureRefused:
			return Result{
				Outcome:           OutcomeRefused,
				SignatureID:       id,
				Refusal:           Refusal(row.RefusalReason),
				RetryAfterSeconds: row.RetryAfterSeconds,
				Limit:             row.RefusalLimit,
				SignerStatus:      row.SignerStatus,
				SignerReason:      row.SignerStatusReason,
			}, nil
		}
		select {
		case <-ring:
		case <-ticker.C:
		case <-deadline.C:
			// The row stays a request; a pod that reaches it later closes
			// it as unanswered rather than signing what nobody waits for.
			return Result{Outcome: OutcomeUnanswered, SignatureID: id}, nil
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}
}
