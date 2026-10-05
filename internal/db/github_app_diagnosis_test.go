package db

import (
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestRecordAppDiagnosis pins that a diagnosis reaches the caller as one only
// when it was stored. A *GitHubAppUnusableError is read as "the registration
// says so" — routes answer 422, the poller treats the App as recorded — so a
// diagnosis the write lost must come back as the write's failure.
func TestRecordAppDiagnosis(t *testing.T) {
	listErr := errors.New("githubapp: list installations: status 404")
	diagnosis := &GitHubAppUnusableError{Reason: domain.GitHubAppMissing, Err: listErr}

	t.Run("stored", func(t *testing.T) {
		var recorded domain.GitHubAppUnusableReason
		err := RecordAppDiagnosis(diagnosis, func(r domain.GitHubAppUnusableReason) error {
			recorded = r
			return nil
		})
		var unusable *GitHubAppUnusableError
		if !errors.As(err, &unusable) || unusable.Reason != domain.GitHubAppMissing {
			t.Errorf("error = %v; want the stored diagnosis", err)
		}
		if recorded != domain.GitHubAppMissing {
			t.Errorf("recorded %q; want %q", recorded, domain.GitHubAppMissing)
		}
	})

	t.Run("the write fails", func(t *testing.T) {
		writeErr := errors.New("database is locked")
		err := RecordAppDiagnosis(diagnosis, func(domain.GitHubAppUnusableReason) error { return writeErr })
		var unusable *GitHubAppUnusableError
		if errors.As(err, &unusable) {
			t.Errorf("error = %v still reads as a recorded diagnosis; want the write's failure", err)
		}
		if !errors.Is(err, writeErr) {
			t.Errorf("error = %v; want it to wrap the write's failure", err)
		}
	})

	t.Run("not a diagnosis", func(t *testing.T) {
		called := false
		err := RecordAppDiagnosis(listErr, func(domain.GitHubAppUnusableReason) error {
			called = true
			return nil
		})
		if err != listErr || called {
			t.Errorf("error = %v, recorder called = %v; want the error unchanged and nothing recorded", err, called)
		}
	})
}
