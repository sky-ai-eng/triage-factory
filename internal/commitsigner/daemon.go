package commitsigner

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/commitsign"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// DaemonRefusal is a commit the daemon holding it refused before any signer
// was asked (CheckCommit): one whose committer is not the run's signer, or
// one that is not a commit. In multi mode that daemon is the run's sidecar,
// which holds no database and records no metrics, so it relays the refusal to
// its orchestrator, which records it here.
type DaemonRefusal struct {
	OrgID          string
	ConversationID string

	// Signer, SignerHost and SignerUserID are the signer the run was sealed
	// with: the kind the refusal is counted under, and the account its
	// commit_sign_denied row is about.
	Signer       domain.CommitSigningMode
	SignerHost   string
	SignerUserID int64

	// Refusal is RefusalCommitterMismatch or RefusalNotACommit. Committer is
	// the committer a mismatched commit named.
	Refusal   Refusal
	Committer commitsign.Identity

	// DedupKey keys the commit_sign_denied row, so a refusal recorded twice
	// writes one row.
	DedupKey string
}

// RecordDaemonRefusal counts r in tf_commit_signatures_total and, for a
// committer mismatch, logs it and writes its commit_sign_denied row, as the
// local signer does for the refusals it makes itself. Best-effort, like every
// refusal's audit row: the refusal was answered before it was recorded.
func RecordDaemonRefusal(ctx context.Context, stores db.Stores, r DaemonRefusal) {
	countRequest(r.Signer, r.OrgID, OutcomeRefused, r.Refusal)
	if r.Refusal != RefusalCommitterMismatch {
		return
	}
	signerLog.WarnContext(ctx, "refused to sign a commit whose committer is not the run's signer",
		"org", r.OrgID, "conversation", r.ConversationID, "committer", formatIdent(r.Committer))
	recordDenied(ctx, stores, denial{
		orgID:          r.OrgID,
		conversationID: r.ConversationID,
		signerHost:     r.SignerHost,
		signerUserID:   r.SignerUserID,
		reason:         deniedCommitterMismatch,
		committer:      formatIdent(r.Committer),
		dedupKey:       r.DedupKey,
	})
}
