package agenthost

import (
	"context"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// stampedClaimCreds is a ClaimCredentialsStore stub answering Get from fixed
// values — the seam that lets these tests exercise the stamped-manifest arm
// without a Postgres claim_credentials table behind it.
type stampedClaimCreds struct {
	bundle db.SealedBundle
	found  bool
	err    error
}

func (s stampedClaimCreds) Put(context.Context, string, string, string, int64, []byte, []string) error {
	return nil
}

func (s stampedClaimCreds) Get(context.Context, string, string) (db.SealedBundle, bool, error) {
	return s.bundle, s.found, s.err
}

// TestDirectRuntime_AvailableSources_StampedManifestWins pins the first door:
// where the claim carries a stamped tools manifest, the runtime answers from
// it verbatim and never reaches the live resolve — the same precedence the
// spawner's toolsReferenceFor applies, which is what keeps a run's help index
// and its <tools> section derived from one answer. An empty non-nil manifest
// is a real answer ("nothing available"), not a fall-through.
func TestDirectRuntime_AvailableSources_StampedManifestWins(t *testing.T) {
	for _, tt := range []struct {
		name     string
		manifest []string
	}{
		{"stamped kinds", []string{"github", "slack"}},
		{"empty manifest is a real answer", []string{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rt := newDirectRuntime(db.Stores{
				ClaimCredentials: stampedClaimCreds{bundle: db.SealedBundle{IncludeTools: tt.manifest}, found: true},
			}, ConversationInfo{OrgID: "org-1", ConversationID: "conversation-1"})

			kinds, err := rt.AvailableSources(context.Background())
			if err != nil {
				t.Fatalf("AvailableSources: %v", err)
			}
			if len(kinds) != len(tt.manifest) {
				t.Fatalf("kinds = %v, want the stamped manifest %v", kinds, tt.manifest)
			}
			for i := range kinds {
				if kinds[i] != tt.manifest[i] {
					t.Fatalf("kinds = %v, want the stamped manifest %v", kinds, tt.manifest)
				}
			}
		})
	}
}

// erroringSecrets fails every system secret read — the deterministic stand-in
// for a placement where the live availability resolve cannot answer (the
// executor's disabled secret store). Only GetSystem is on this path; the
// embedded nil interface guards the rest structurally.
type erroringSecrets struct{ db.SecretStore }

func (erroringSecrets) GetSystem(context.Context, string, string) (string, error) {
	return "", errors.New("secret store disabled on this placement")
}

// TestDirectRuntime_AvailableSources_HardReadErrorFallsThrough pins the
// degrade shape of a manifest read that fails hard (not the local-mode
// refusal): the runtime falls through to the live resolve rather than
// answering with the Get error — and where that resolve cannot answer either
// (this placement's secret store refuses), the resolve's own error is what
// surfaces, which the CLI's help route renders as the unfiltered index.
func TestDirectRuntime_AvailableSources_HardReadErrorFallsThrough(t *testing.T) {
	readErr := errors.New("claim_credentials read failed")
	rt := newDirectRuntime(db.Stores{
		ClaimCredentials: stampedClaimCreds{err: readErr},
		Secrets:          erroringSecrets{},
	}, ConversationInfo{OrgID: "org-1", ConversationID: "conversation-1"})

	_, err := rt.AvailableSources(context.Background())
	if err == nil {
		t.Fatal("expected the live resolve's error on a placement that cannot answer")
	}
	if errors.Is(err, readErr) {
		t.Fatal("the manifest read's error was returned as the answer; it must fall through to the live resolve")
	}
}

// TestRelayRuntime_AvailableSourcesRoundTrips pins the sidecar placement: the
// op relays to the orchestrator-side RelayServer and comes back with the
// directRuntime's answer — here the stamped manifest — through the same
// JSON envelope the wire uses.
func TestRelayRuntime_AvailableSourcesRoundTrips(t *testing.T) {
	stores, info := newCaptureStores(t, true)
	stores.ClaimCredentials = stampedClaimCreds{
		bundle: db.SealedBundle{IncludeTools: []string{"github", "jira", "slack"}},
		found:  true,
	}
	srv := NewRelayServer(stores, info, nil)
	rt := newRelayRuntime(directDispatchConn{srv: srv}, info, nil)

	kinds, err := rt.AvailableSources(context.Background())
	if err != nil {
		t.Fatalf("AvailableSources relay: %v", err)
	}
	want := []string{"github", "jira", "slack"}
	if len(kinds) != len(want) {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("kinds = %v, want %v", kinds, want)
		}
	}
}
