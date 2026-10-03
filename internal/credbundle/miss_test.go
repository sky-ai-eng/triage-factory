package credbundle_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/credbundle"
)

func TestMissReason(t *testing.T) {
	cases := []struct {
		sentinel error
		want     string
	}{
		{credbundle.ErrNoBundle, credbundle.MissNoBundle},
		{credbundle.ErrNoRepoToken, credbundle.MissNoRepoToken},
		{credbundle.ErrNoCLIToken, credbundle.MissNoCLIToken},
		{credbundle.ErrNoJiraCredential, credbundle.MissNoJiraCredential},
		{credbundle.ErrTokenExpiring, credbundle.MissTokenExpiring},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			if got := credbundle.MissReason(tc.sentinel); got != tc.want {
				t.Errorf("bare: MissReason = %q, want %q", got, tc.want)
			}
			// The git proxy's source keeps agentproc's sentinel beside this one,
			// and the proxy wraps the source's error again on the way up.
			wrapped := fmt.Errorf("token source: %w",
				fmt.Errorf("%w: repo o/r: %w", agentproc.ErrNoGitCredentials, tc.sentinel))
			if got := credbundle.MissReason(wrapped); got != tc.want {
				t.Errorf("double-wrapped: MissReason = %q, want %q", got, tc.want)
			}
			if !errors.Is(wrapped, agentproc.ErrNoGitCredentials) {
				t.Error("double-wrapping lost agentproc.ErrNoGitCredentials")
			}
		})
	}

	t.Run("unknown error is other", func(t *testing.T) {
		for _, err := range []error{
			errors.New("mint installation token: 500"),
			agentproc.ErrNoGitCredentials,
			nil,
		} {
			if got := credbundle.MissReason(err); got != credbundle.MissOther {
				t.Errorf("MissReason(%v) = %q, want %q", err, got, credbundle.MissOther)
			}
		}
	})
}
