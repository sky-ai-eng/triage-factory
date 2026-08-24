package repoprofile

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestProfiler_CloneProtocol_ResolvesAgainstMode pins the clone URL the
// profiler persists to domain.EffectiveCloneProtocol rather than to the stored
// column, for the same stored value in both modes.
//
// The stored value is the interesting part: "ssh" is what a row carries when
// nobody has chosen anything, so a multi-mode org that honored it would write
// git@github.com URLs onto every repository row — and an SSH remote can carry
// no App installation token, so every host-side clone fails on a runtime with
// no key, agent or known_hosts. Local honors the same value, because there SSH
// is a real credential path the operator may have set up.
func TestProfiler_CloneProtocol_ResolvesAgainstMode(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mode  runmode.Mode
		store string
		want  string
	}{
		{"multi refuses the stored ssh", runmode.ModeMulti, "ssh", "https://x/acme/api.git"},
		{"multi refuses an unset row", runmode.ModeMulti, "", "https://x/acme/api.git"},
		{"local honors the stored ssh", runmode.ModeLocal, "ssh", "git@x:acme/api.git"},
		{"local defaults to https", runmode.ModeLocal, "https", "https://x/acme/api.git"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runmode.SetForTest(t, tc.mode)

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/contents/") {
					w.WriteHeader(http.StatusNotFound) // docs-less: one meta fetch, one upsert
					return
				}
				_, _ = w.Write([]byte(`{"default_branch":"main",` +
					`"clone_url":"https://x/acme/api.git","ssh_url":"git@x:acme/api.git"}`))
			}))
			defer srv.Close()

			repos := &fetchRepositoryStore{names: []string{"acme/api"}}
			p := NewProfiler(
				fixedResolver{client: github.NewClient(srv.URL, "tok")},
				nil, nil,
				repos,
				cloneProtocolOrgStore{stored: tc.store},
				nil, nil, nil,
			)

			if err := p.Run(context.Background(), true /* force: bypass the TTL read */); err != nil {
				t.Fatalf("Run: %v", err)
			}

			got, ok := repos.upsertedByID()["acme/api"]
			if !ok {
				t.Fatal("repo was not upserted; want persisted with a clone URL")
			}
			if got.CloneURL != tc.want {
				t.Errorf("clone_url = %q; want %q (stored %q in %s mode)",
					got.CloneURL, tc.want, tc.store, tc.mode)
			}
		})
	}
}

// cloneProtocolOrgStore is oneOrgStore with the clone-protocol column under
// the test's control.
type cloneProtocolOrgStore struct {
	db.OrgsStore
	stored string
}

func (cloneProtocolOrgStore) ListActiveSystem(context.Context) ([]string, error) {
	return []string{"org-1"}, nil
}

// BackgroundJobsModel and the Anthropic ref are both set because the cycle
// resolves them from this same read and skips entirely without a usable model
// or a credential to run it on — the clone URL this test is about is written
// past both gates. The ref matters in multi specifically, where an org that has
// bound nothing can authenticate nothing.
//
// The model is spelled per mode, because the accepted set is the deployment's
// own universe: a multi org stores the native wire id its runtime sends, a local
// one the harness alias its subprocess resolves. This store answers for both
// arms of the table, so it carries whichever the mode under test would.
func (s cloneProtocolOrgStore) GetSettingsSystem(context.Context, string) (domain.OrgSettings, error) {
	model := domain.LocalBackgroundJobsModel
	if runmode.Current() == runmode.ModeMulti {
		model = domain.ModelHaiku
	}
	return domain.OrgSettings{
		GitHubCloneProtocol: s.stored,
		BackgroundJobsModel: model,
		AnthropicAPIKeyRef:  "anthropic_api_key",
	}, nil
}
