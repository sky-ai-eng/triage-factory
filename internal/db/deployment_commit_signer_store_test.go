package db

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestDeploymentKeyCheckTransition walks every status a key check can find
// the deployment signer in, with the key found and not.
func TestDeploymentKeyCheckTransition(t *testing.T) {
	for _, tc := range []struct {
		name       string
		from       domain.CommitSignerStatus
		fromReason string
		found      bool
		want       domain.CommitSignerStatus
		wantReason string
	}{
		{"pending, key found", domain.CommitSignerPending, "", true, domain.CommitSignerActive, ""},
		{"pending, key missing", domain.CommitSignerPending, "", false, domain.CommitSignerPending, ""},
		{"active, key found", domain.CommitSignerActive, "", true, domain.CommitSignerActive, ""},
		{"active, key missing", domain.CommitSignerActive, "", false, domain.CommitSignerBroken, domain.CommitSignerReasonKeyMissing},
		{"broken for its key, key back", domain.CommitSignerBroken, domain.CommitSignerReasonKeyMissing, true, domain.CommitSignerActive, ""},
		{"broken by a commit, key found", domain.CommitSignerBroken, "unverified_email", true, domain.CommitSignerBroken, "unverified_email"},
	} {
		if got, reason := DeploymentKeyCheckTransition(tc.from, tc.fromReason, tc.found); got != tc.want || reason != tc.wantReason {
			t.Errorf("%s: = %s (%q), want %s (%q)", tc.name, got, reason, tc.want, tc.wantReason)
		}
	}
}
