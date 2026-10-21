package db

import (
	"slices"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestReconcileInstallations_RemovesBeforeItUpserts pins the order the
// one-live-installation-per-account key needs. An account that uninstalled and
// reinstalled without TF seeing the uninstall is listed under a new
// installation id while its old row is still live, and the new row can land
// only once the old one is removed; an installation still listed is never
// removed.
func TestReconcileInstallations_RemovesBeforeItUpserts(t *testing.T) {
	var calls []string
	err := ReconcileInstallations(
		[]domain.OrgGitHubAppInstallation{
			{InstallationID: "new", AccountID: "1234"},
			{InstallationID: "kept", AccountID: "5678"},
		},
		[]string{"old", "kept"},
		func(inst domain.OrgGitHubAppInstallation) error {
			calls = append(calls, "upsert "+inst.InstallationID)
			return nil
		},
		func(id string) error {
			calls = append(calls, "remove "+id)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("ReconcileInstallations: %v", err)
	}
	if want := []string{"remove old", "upsert new", "upsert kept"}; !slices.Equal(calls, want) {
		t.Errorf("calls = %v, want %v", calls, want)
	}
}
