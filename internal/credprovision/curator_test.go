package credprovision

import (
	"context"
	"encoding/base64"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/credbundle"
	"github.com/sky-ai-eng/triage-factory/internal/credseal"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
	"github.com/sky-ai-eng/triage-factory/internal/llmcred"
)

// --- fakes for the curator-turn provisioning path ---

type fakeCurator struct {
	db.CuratorStore
	turn *domain.CuratorTurnProvision
	ok   bool
}

func (f *fakeCurator) GetTurnProvisionInfoSystem(context.Context, string, string) (*domain.CuratorTurnProvision, bool, error) {
	return f.turn, f.ok, nil
}

type fakeInstances struct {
	db.InstanceStore
	inst *domain.Instance
}

func (f *fakeInstances) Get(context.Context, string) (*domain.Instance, error) { return f.inst, nil }

type fakeProjects struct {
	db.ProjectStore
	project *domain.Project
}

func (f *fakeProjects) GetSystem(context.Context, string, string) (*domain.Project, error) {
	return f.project, nil
}

type putCall struct {
	orgID, conversationID, executorID string
	bootEpoch                         int64
	sealed                            []byte
}

type fakeClaimCredentials struct {
	db.ClaimCredentialsStore
	puts []putCall
}

func (f *fakeClaimCredentials) Put(_ context.Context, orgID, conversationID, executorID string, bootEpoch int64, sealed []byte, _ []string) error {
	f.puts = append(f.puts, putCall{orgID, conversationID, executorID, bootEpoch, sealed})
	return nil
}

type fakeLLM struct {
	mat llmcred.Material
	// gotModel records the model the provisioner selected material with — the
	// provider selector, so a test can assert the conversation's own model
	// reaches resolution rather than the org's preferred provider.
	gotModel string
}

func (f *fakeLLM) ResolveForBundle(_ context.Context, _, _, model string) (llmcred.Material, error) {
	f.gotModel = model
	return f.mat, nil
}

type fakeConversations struct {
	db.ConversationStore
	conv *domain.Conversation
}

func (f *fakeConversations) GetSystem(context.Context, string, string) (*domain.Conversation, error) {
	return f.conv, nil
}

// curatorTurnManager builds a Manager wired with the curator fakes plus a
// keypair the test can unseal the written bundle with.
func curatorTurnManager(t *testing.T, turn *domain.CuratorTurnProvision, project *domain.Project, tracked map[string]bool) (*Manager, *fakeClaimCredentials, *credseal.KeyPair) {
	t.Helper()
	kp, err := credseal.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	if turn != nil && turn.CredPubKey == "seal-to-me" {
		turn.CredPubKey = base64.StdEncoding.EncodeToString(kp.Public[:])
	}
	rc := &fakeClaimCredentials{}
	m := &Manager{
		stores: db.Stores{
			Curator:          &fakeCurator{turn: turn, ok: turn != nil},
			Conversations:    &fakeConversations{conv: &domain.Conversation{ID: "conv-1", Model: domain.ModelSonnet}},
			Instances:        &fakeInstances{inst: &domain.Instance{BootEpoch: 7}},
			Projects:         &fakeProjects{project: project},
			TeamGitHubRepos:  &fakeTeamRepos{tracked: tracked},
			ClaimCredentials: rc,
		},
		ghResolver: &fakeScopedResolver{
			base:    "https://ghe.example",
			name:    "acme[bot]",
			email:   "acme[bot]@users.noreply.github.com",
			hasCred: true,
			token:   githubapp.Token{Value: "ghs_curator"},
		},
		llm: &fakeLLM{mat: llmcred.Material{Env: map[string]string{"ANTHROPIC_API_KEY": "sk-test"}}},
	}
	return m, rc, kp
}

// TestProvisionForCuratorTurn_SealsPinnedIntersectTracked pins the happy path:
// the bundle's GitHub authorized set is the project's pinned repos ∩ the
// conversation-snapshot team's tracked set (an untracked pinned repo is
// filtered out), it seals to the active claim's published pubkey and the
// home's boot epoch, and the orchestrator can't read it (only the keypair's
// private half unseals). The write lands on the shared claim_credentials
// channel keyed by the conversation id.
func TestProvisionForCuratorTurn_SealsPinnedIntersectTracked(t *testing.T) {
	turn := &domain.CuratorTurnProvision{
		ConversationID: "conv-1", OrgID: "org-1", ProjectID: "proj-1", TeamID: "team-1",
		HomeInstanceID: "home-1", CredPubKey: "seal-to-me",
	}
	project := &domain.Project{TeamID: "team-1", PinnedRepos: []string{"acme/widgets", "acme/untracked"}}
	m, rc, kp := curatorTurnManager(t, turn, project, map[string]bool{"acme/widgets": true})

	if err := m.ProvisionForCuratorTurn(context.Background(), "org-1", "conv-1"); err != nil {
		t.Fatalf("ProvisionForCuratorTurn: %v", err)
	}
	if len(rc.puts) != 1 {
		t.Fatalf("Put called %d times, want 1", len(rc.puts))
	}
	p := rc.puts[0]
	if p.orgID != "org-1" || p.conversationID != "conv-1" || p.executorID != "home-1" || p.bootEpoch != 7 {
		t.Errorf("Put args = (%q, %q, %q, %d), want (org-1, conv-1, home-1, 7)", p.orgID, p.conversationID, p.executorID, p.bootEpoch)
	}
	// The conversation's own model is what selected the sealed material: an org
	// holding both providers seals the one this turn runs on, not a preferred one.
	if got := m.llm.(*fakeLLM).gotModel; got != domain.ModelSonnet {
		t.Errorf("LLM material resolved with model %q, want the conversation's %q", got, domain.ModelSonnet)
	}

	plaintext, err := kp.Open(p.sealed)
	if err != nil {
		t.Fatalf("Open sealed bundle: %v", err)
	}
	bundle, err := credbundle.Unmarshal(plaintext)
	if err != nil {
		t.Fatalf("unmarshal bundle: %v", err)
	}
	if bundle.BootEpoch != 7 {
		t.Errorf("bundle BootEpoch = %d, want 7", bundle.BootEpoch)
	}
	if bundle.LLM["ANTHROPIC_API_KEY"] != "sk-test" {
		t.Errorf("bundle LLM = %v, want the resolved key", bundle.LLM)
	}
	if bundle.GitHub == nil {
		t.Fatal("bundle GitHub is nil, want the minted repo set")
	}
	if _, ok := bundle.GitHub.RepoTokens["acme/widgets"]; !ok {
		t.Errorf("RepoTokens missing acme/widgets; got %v", bundle.GitHub.RepoTokens)
	}
	if _, ok := bundle.GitHub.RepoTokens["acme/untracked"]; ok {
		t.Error("minted a token for an untracked pinned repo — pinned ∩ tracked must gate it")
	}
}

// TestProvisionForCuratorTurn_NoOps pins the tolerant no-op cases: an
// un-homed or keyless active claim (and a conversation with NO active claim
// — the turn finished or was never claimed) writes nothing.
func TestProvisionForCuratorTurn_NoOps(t *testing.T) {
	project := &domain.Project{TeamID: "team-1", PinnedRepos: []string{"acme/widgets"}}
	cases := []struct {
		name string
		turn *domain.CuratorTurnProvision
	}{
		{"un-homed", &domain.CuratorTurnProvision{ConversationID: "c", OrgID: "org-1", ProjectID: "proj-1", TeamID: "team-1", HomeInstanceID: "", CredPubKey: "seal-to-me"}},
		{"keyless", &domain.CuratorTurnProvision{ConversationID: "c", OrgID: "org-1", ProjectID: "proj-1", TeamID: "team-1", HomeInstanceID: "home-1", CredPubKey: ""}},
		{"no_active_claim", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, rc, _ := curatorTurnManager(t, tc.turn, project, map[string]bool{"acme/widgets": true})
			if err := m.ProvisionForCuratorTurn(context.Background(), "org-1", "c"); err != nil {
				t.Fatalf("ProvisionForCuratorTurn: %v", err)
			}
			if len(rc.puts) != 0 {
				t.Errorf("Put called %d times, want 0 (no-op)", len(rc.puts))
			}
		})
	}
}
