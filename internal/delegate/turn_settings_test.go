package delegate

import (
	"context"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestResolveTurnSettings is the send's settings decision, case by case, on
// the native catalog: what a message names is stamped, what it leaves out is
// inherited from the newest person row or else taken from the claim, and each
// refusal names its field.
func TestResolveTurnSettings(t *testing.T) {
	const (
		stepModel     = domain.ModelSonnet               // claude-sonnet-5: all five efforts
		teamDefault   = "claude-sonnet-5-5"              // all five efforts
		fable         = "claude-fable-5-2"               // all five efforts
		bedrockSonnet = "us.anthropic.claude-sonnet-5-5" // ceiling xhigh
		disabled      = domain.ModelOpus
	)
	enabled := []string{stepModel, teamDefault, fable, bedrockSonnet}

	type row struct {
		role            string
		model, effort   string
		occupancyTokens int
	}
	cases := []struct {
		name    string
		running bool
		// blueprint, when set, is the blueprint's status in place of running
		// or completed, or "unreadable" for a read of it that fails.
		blueprint string
		// enabled, when set, is the team's set in place of the default one.
		enabled  []string
		rows     []row
		explicit domain.TurnSettings
		want     domain.TurnSettings
		wantErr  error
		field    string
	}{
		{name: "explicit settings are stamped",
			explicit: domain.TurnSettings{Model: fable, Effort: domain.EffortHigh},
			want:     domain.TurnSettings{Model: fable, Effort: domain.EffortHigh}},
		{name: "absent settings take the newest person row's",
			rows: []row{{role: "user", model: stepModel, effort: domain.EffortMax}, {role: "user", model: fable, effort: domain.EffortLow}},
			want: domain.TurnSettings{Model: fable, Effort: domain.EffortLow}},
		{name: "a given field wins and the other is inherited",
			rows:     []row{{role: "user", model: fable, effort: domain.EffortLow}},
			explicit: domain.TurnSettings{Effort: domain.EffortHigh},
			want:     domain.TurnSettings{Model: fable, Effort: domain.EffortHigh}},
		{name: "with no person row the model is the claim's and the effort empty",
			want: domain.TurnSettings{Model: teamDefault}},
		{name: "a person row that recorded no settings is no person row",
			rows: []row{{role: "user"}},
			want: domain.TurnSettings{Model: teamDefault}},
		{name: "a running step runs on its model and no effort",
			running: true, rows: []row{{role: "user", model: fable, effort: domain.EffortLow}},
			want: domain.TurnSettings{Model: stepModel}},
		{name: "a running step takes its own model named",
			running: true, explicit: domain.TurnSettings{Model: stepModel},
			want: domain.TurnSettings{Model: stepModel}},
		{name: "a running step refuses another model",
			running: true, explicit: domain.TurnSettings{Model: fable},
			wantErr: ErrStepSettingsFixed, field: "model"},
		{name: "a running step refuses an effort",
			running: true, explicit: domain.TurnSettings{Effort: domain.EffortLow},
			wantErr: ErrStepSettingsFixed, field: "effort"},
		{name: "a running step runs on its model after the team stops enabling it",
			running: true, enabled: []string{teamDefault, fable},
			want: domain.TurnSettings{Model: stepModel}},
		{name: "the step an aborted blueprint stopped on runs on its model",
			blueprint: "aborted", rows: []row{{role: "user", model: fable, effort: domain.EffortLow}},
			want: domain.TurnSettings{Model: stepModel}},
		{name: "the step an aborted blueprint stopped on refuses another model",
			blueprint: "aborted", explicit: domain.TurnSettings{Model: fable},
			wantErr: ErrStepSettingsFixed, field: "model"},
		{name: "a blueprint that cannot be read refuses the message",
			blueprint: "unreadable", explicit: domain.TurnSettings{Model: fable},
			wantErr: errBlueprintUnreadable},
		{name: "a model outside the team's set is refused",
			explicit: domain.TurnSettings{Model: disabled},
			wantErr:  domain.ErrModelNotEnabled, field: "model"},
		{name: "an inherited model outside the team's set is refused",
			rows:    []row{{role: "user", model: disabled}},
			wantErr: domain.ErrModelNotEnabled, field: "model"},
		{name: "an explicit effort the model does not take is refused",
			explicit: domain.TurnSettings{Model: bedrockSonnet, Effort: domain.EffortMax},
			wantErr:  ErrEffortNotSupported, field: "effort"},
		{name: "an inherited effort the model does not take is dropped",
			rows:     []row{{role: "user", model: stepModel, effort: domain.EffortMax}},
			explicit: domain.TurnSettings{Model: bedrockSonnet},
			want:     domain.TurnSettings{Model: bedrockSonnet}},
		{name: "a switch past the new model's compaction threshold is refused",
			rows:     []row{{role: "user", model: stepModel}, {role: "assistant", model: stepModel, occupancyTokens: 850_000}},
			explicit: domain.TurnSettings{Model: fable},
			wantErr:  ErrModelWindowTooSmall, field: "model"},
		{name: "a switch below the threshold is taken",
			rows:     []row{{role: "user", model: stepModel}, {role: "assistant", model: stepModel, occupancyTokens: 700_000}},
			explicit: domain.TurnSettings{Model: fable},
			want:     domain.TurnSettings{Model: fable}},
		{name: "staying on the model is never a switch",
			rows: []row{{role: "user", model: stepModel}, {role: "assistant", model: stepModel, occupancyTokens: 990_000}},
			want: domain.TurnSettings{Model: stepModel}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runmode.SetForTest(t, runmode.ModeMulti)
			database := newDelegateTestDB(t)
			connectProviders(t, database, true, true)
			seedConversation(t, database, "r-settings", "sess", t.TempDir())
			if _, err := database.Exec(`UPDATE conversations SET model=? WHERE id='r-settings'`, stepModel); err != nil {
				t.Fatalf("set the step's model: %v", err)
			}
			switch {
			case tc.blueprint == "aborted":
				finishBlueprint(t, database, blueprintRunIDForConversation(t, database, "r-settings"), "aborted", 0)
			case tc.blueprint == "" && !tc.running:
				finishBlueprint(t, database, blueprintRunIDForConversation(t, database, "r-settings"), "completed", 0)
			}
			stores := testSpawnerStores(database)
			if tc.blueprint == "unreadable" {
				stores.Blueprints = unreadableBlueprint{stores.Blueprints}
			}
			set := enabled
			if tc.enabled != nil {
				set = tc.enabled
			}
			s := NewSpawner(database, stores, nil, nil, "m")
			s.SetRunCredentialResolvers(nil, nil, func(context.Context, string, string) (domain.TeamModels, error) {
				return domain.NewTeamModels(teamDefault, domain.TeamModelSet(set, orgUnrestricted())), nil
			})
			ctx := context.Background()
			for _, r := range tc.rows {
				msg := &domain.Message{ConversationID: "r-settings", Role: r.role, Content: "x", Model: r.model, Effort: r.effort}
				if r.role == "user" {
					msg.UserID = runmode.LocalDefaultUserID
				}
				if r.occupancyTokens > 0 {
					n := r.occupancyTokens
					msg.InputTokens = &n
				}
				if _, err := s.conversations.InsertMessageSystem(ctx, runmode.LocalDefaultOrgID, msg); err != nil {
					t.Fatalf("insert row: %v", err)
				}
			}
			conv, err := s.conversations.GetSystem(ctx, runmode.LocalDefaultOrgID, "r-settings")
			if err != nil || conv == nil {
				t.Fatalf("load conversation: %v", err)
			}

			got, err := s.resolveTurnSettings(ctx, runmode.LocalDefaultOrgID, *conv, tc.explicit)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				var fe *SettingsFieldError
				if named := errors.As(err, &fe); named != (tc.field != "") || (named && fe.Field != tc.field) {
					t.Errorf("err = %v, want it to name %q", err, tc.field)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveTurnSettings: %v", err)
			}
			if got != tc.want {
				t.Errorf("settings = %+v, want %+v", got, tc.want)
			}
		})
	}
}

var errBlueprintUnreadable = errors.New("blueprint read failed")

// unreadableBlueprint is a blueprint store whose run reads fail.
type unreadableBlueprint struct {
	db.BlueprintStore
}

func (unreadableBlueprint) GetRunSystem(context.Context, string, string) (*domain.BlueprintRun, error) {
	return nil, errBlueprintUnreadable
}

// TestSendMessage_AnEndedConversationIsRefusedBeforeItsSettings: a send the
// conversation's own state refuses is answered with that refusal, not with
// the settings it named. A person told to pick another model, only to learn
// the conversation has ended, did work for nothing.
func TestSendMessage_AnEndedConversationIsRefusedBeforeItsSettings(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	org := runmode.LocalDefaultOrgID
	ctx := context.Background()
	database := newDelegateTestDB(t)
	const conversationID = "r-ended-settings"
	seedConversation(t, database, conversationID, "sess", t.TempDir())
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
	s.SetRunCredentialResolvers(nil, nil, func(context.Context, string, string) (domain.TeamModels, error) {
		return domain.NewTeamModels(domain.ModelSonnet, domain.TeamModelSet([]string{domain.ModelSonnet}, orgUnrestricted())), nil
	})
	if fenced := s.failConversation(org, conversationID, "", holderClaimFor(t, s, org, conversationID), "the runtime died", domain.ConversationFailureUnclassified); fenced {
		t.Fatal("failConversation reported fenced")
	}

	err := s.SendMessage(ctx, org, conversationID, runmode.LocalDefaultUserID, "retry", domain.TurnSettings{Model: domain.ModelOpus})
	if !errors.Is(err, ErrConversationEnded) {
		t.Errorf("SendMessage = %v, want ErrConversationEnded ahead of the model it named", err)
	}
}
