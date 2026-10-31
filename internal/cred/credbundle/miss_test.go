package credbundle_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/cred/credbundle"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

func TestMissReason(t *testing.T) {
	cases := []struct {
		sentinel error
		want     string
	}{
		{credbundle.ErrNoBundle, credbundle.MissNoBundle},
		{credbundle.ErrNoRepoToken, credbundle.MissNoRepoToken},
		{credbundle.ErrNoCLIToken, credbundle.MissNoCLIToken},
		{&credbundle.SourceMissError{Kind: "linear", State: domain.SourceUnresolved}, credbundle.MissSourceUnresolved},
		{&credbundle.SourceMissError{Kind: "jira", State: domain.SourceUnconfigured}, credbundle.MissSourceUnconfigured},
		{&credbundle.SourceMissError{Kind: "jira", State: domain.SourceDisabled}, credbundle.MissSourceDisabled},
		{&credbundle.SourceMissError{Kind: "slack", State: domain.SourceUnlicensed}, credbundle.MissSourceUnlicensed},
		{&credbundle.SourceMissError{Kind: "slack", State: domain.SourceWithheld}, credbundle.MissSourceWithheld},
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
			// A sealed source never misses; a miss claiming one is not a reason.
			&credbundle.SourceMissError{Kind: "linear", State: domain.SourceSealed},
			nil,
		} {
			if got := credbundle.MissReason(err); got != credbundle.MissOther {
				t.Errorf("MissReason(%v) = %q, want %q", err, got, credbundle.MissOther)
			}
		}
	})
}

// TestSourceMiss pins the miss a proxy builds from the held manifest: the
// entry's state, unconfigured for a source the manifest does not name, and
// the source carried beside the reason.
func TestSourceMiss(t *testing.T) {
	m := domain.SourceManifest{
		"linear": {State: domain.SourceUnresolved, Reason: domain.SourceReasonTokenEndpointUnavailable},
	}
	err := fmt.Errorf("linear api proxy: %w", credbundle.SourceMiss(m, "linear"))
	if got := credbundle.MissReason(err); got != credbundle.MissSourceUnresolved {
		t.Errorf("MissReason = %q, want %q", got, credbundle.MissSourceUnresolved)
	}
	if got := credbundle.MissSource(err); got != "linear" {
		t.Errorf("MissSource = %q, want linear", got)
	}
	if got := credbundle.MissReason(credbundle.SourceMiss(m, "jira")); got != credbundle.MissSourceUnconfigured {
		t.Errorf("MissReason for an unnamed source = %q, want %q", got, credbundle.MissSourceUnconfigured)
	}
	if got := credbundle.MissReason(credbundle.SourceMiss(nil, "jira")); got != credbundle.MissSourceUnconfigured {
		t.Errorf("MissReason with no manifest = %q, want %q", got, credbundle.MissSourceUnconfigured)
	}
	if got := credbundle.MissSource(credbundle.ErrNoRepoToken); got != "" {
		t.Errorf("MissSource(no repo token) = %q, want empty", got)
	}
	for _, state := range []domain.SourceState{domain.SourceUnresolved, domain.SourceUnconfigured, domain.SourceDisabled, domain.SourceUnlicensed, domain.SourceWithheld} {
		reason := credbundle.MissReason(&credbundle.SourceMissError{Kind: "jira", State: state})
		if got, ok := credbundle.StateForMissReason(reason); !ok || got != state {
			t.Errorf("StateForMissReason(%q) = %q, %v; want %q", reason, got, ok, state)
		}
	}
	if _, ok := credbundle.StateForMissReason(credbundle.MissNoBundle); ok {
		t.Error("StateForMissReason(no_bundle) answered a state")
	}
}
