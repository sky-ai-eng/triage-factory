package commitsigner

import (
	"context"

	"go.opentelemetry.io/otel/metric"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
)

// UnsignedTip is a push audit row's signer when no signature of the org's
// produced the commit it pushed.
const UnsignedTip = "unsigned"

// TipSigner names the account that signed sha, a commit a push landed or
// tried to land as a ref's tip, for the push's audit row: the login of the
// account whose signature produced it, or UnsignedTip. signed reports which.
// The login is the org's signer's when it is still that account, else the
// account's id in text, as a commit_sign_denied row names it.
func TipSigner(ctx context.Context, stores db.Stores, orgID, sha string) (signer string, signed bool, err error) {
	sig, err := stores.CommitSignatures.ForCommitSystem(ctx, orgID, sha)
	if err != nil {
		return "", false, err
	}
	if sig == nil {
		return UnsignedTip, false, nil
	}
	return signerLogin(ctx, stores.OrgCommitSigners, orgID, sig.SignerUserID), true, nil
}

// CountPush counts one push from a run whose org resolves to a signer, by
// whether the tip it pushed was signed, as tf_commit_signing_pushes_total. An
// unsigned one is a run that committed with signing off.
func CountPush(ctx context.Context, orgID string, signed bool) {
	current().pushes.Add(ctx, 1, metric.WithAttributes(
		telemetry.OrgID(orgID),
		telemetry.Signed(signed),
	))
}
