package dbtest

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// runPersonClaimMintConformance covers the conversation a person's claim
// mints (ConversationStore.MintForPersonClaim) and the "has run" read it is
// told apart from a run conversation by.
func runPersonClaimMintConformance(t *testing.T, mk ConversationStoreFactory) {
	t.Helper()

	newTask := func(t *testing.T, seed ConversationSeeder, suffix string) string {
		t.Helper()
		ent := seed.Entity(t, suffix)
		ev := seed.Event(t, ent, domain.EventGitHubPROpened)
		return seed.Task(t, ent, domain.EventGitHubPROpened, ev)
	}

	// Both doors run one statement, so both are held to the same shape.
	doors := []struct {
		name string
		mint func(store db.ConversationStore, ctx context.Context, orgID, taskID, userID string) (*domain.Conversation, error)
	}{
		{"MintForPersonClaim", func(store db.ConversationStore, ctx context.Context, orgID, taskID, userID string) (*domain.Conversation, error) {
			return store.MintForPersonClaim(ctx, orgID, taskID, userID)
		}},
		{"MintForPersonClaimSystem", func(store db.ConversationStore, ctx context.Context, orgID, taskID, userID string) (*domain.Conversation, error) {
			return store.MintForPersonClaimSystem(ctx, orgID, taskID, userID)
		}},
	}
	for _, door := range doors {
		t.Run(door.name+"_WritesAParkedInteractiveConversation", func(t *testing.T) {
			store, orgID, userID, seed := mk(t)
			ctx := context.Background()
			taskID := newTask(t, seed, "mint-shape")
			// An ended conversation is not the task's any more, so it does
			// not stand in the mint's way.
			prior := seedConversationForTaskTest(t, orgID, taskID, SeedConcluded, seed)
			if _, err := store.EndConversation(ctx, orgID, prior, domain.EndedRequeued); err != nil {
				t.Fatalf("EndConversation: %v", err)
			}

			minted, err := door.mint(store, ctx, orgID, taskID, userID)
			if err != nil {
				t.Fatalf("%s: %v", door.name, err)
			}
			if minted == nil {
				t.Fatalf("%s returned nil on a task with no un-ended conversation", door.name)
			}
			AssertWriteReturnedStoredRow(t, door.name, *minted, func() (*domain.Conversation, error) {
				return store.Get(ctx, orgID, minted.ID)
			})

			if minted.TaskID != taskID {
				t.Errorf("task_id = %q, want %q", minted.TaskID, taskID)
			}
			if minted.Type != domain.ConversationTypeInteractive {
				t.Errorf("type = %q, want %q", minted.Type, domain.ConversationTypeInteractive)
			}
			if minted.Runtime != domain.ConversationRuntimeSDK && minted.Runtime != domain.ConversationRuntimeNative {
				t.Errorf("runtime = %q, want the dialect's runtime stamped explicitly", minted.Runtime)
			}
			if minted.TriggerType != "manual" || minted.CreatorUserID != userID {
				t.Errorf("trigger_type, creator = %q, %q; want manual, %q", minted.TriggerType, minted.CreatorUserID, userID)
			}
			if minted.TeamID == "" {
				t.Error("team_id is empty; a team-visible conversation carries the task's team")
			}
			// Parked with no verdict: live, and not drivable until a person's
			// message arrives.
			if minted.Status != domain.StatusOpen {
				t.Errorf("display status = %q, want %q", minted.Status, domain.StatusOpen)
			}
			if minted.Concluded() {
				t.Error("the minted conversation reads as concluded; it carries no verdict")
			}
			if minted.EndedAt != nil {
				t.Errorf("ended_at = %v, want a live conversation", minted.EndedAt)
			}
			if minted.HasRun {
				t.Error("has_run = true on a conversation with no message")
			}
			if !seed.ParkedAtSet(t, minted.ID) {
				t.Error("parked_at is NULL; the park stamp is what the idle sweeps age the row from")
			}

			stored := seed.StoredShape(t, minted.ID)
			if stored.Type != domain.ConversationTypeInteractive {
				t.Errorf("stored type = %q, want %q", stored.Type, domain.ConversationTypeInteractive)
			}
			// Any origin but the column's 'blueprint' default, which the
			// schema pairs with a blueprint run this row does not have.
			if stored.Origin != domain.ConversationOriginInteractive {
				t.Errorf("stored origin = %q, want %q", stored.Origin, domain.ConversationOriginInteractive)
			}
			if stored.Visibility != "team" {
				t.Errorf("stored visibility = %q, want team", stored.Visibility)
			}
			if stored.Status != domain.StatusOpen {
				t.Errorf("stored status = %q, want %q", stored.Status, domain.StatusOpen)
			}
			if len(stored.NonNull) != 0 {
				t.Errorf("columns the mint leaves NULL hold values: %v", stored.NonNull)
			}

			// It is the task's un-ended conversation now, and the only one.
			live, err := store.ListForTask(ctx, orgID, taskID)
			if err != nil {
				t.Fatalf("ListForTask: %v", err)
			}
			var unended []string
			for _, c := range live {
				if c.EndedAt == nil {
					unended = append(unended, c.ID)
				}
			}
			if len(unended) != 1 || unended[0] != minted.ID {
				t.Errorf("un-ended conversations on the task = %v, want [%s]", unended, minted.ID)
			}
		})

		t.Run(door.name+"_WritesNothingWhenTheTaskHasAnUnendedConversation", func(t *testing.T) {
			store, orgID, userID, seed := mk(t)
			ctx := context.Background()
			// Whatever the status of the one it holds: the boundary decides
			// whose the task is, and a concluded row is still un-ended.
			for _, status := range []string{"", "open", SeedConcluded, "failed"} {
				taskID := newTask(t, seed, "mint-held-"+status)
				held := seedConversationForTaskTest(t, orgID, taskID, status, seed)

				minted, err := door.mint(store, ctx, orgID, taskID, userID)
				if err != nil {
					t.Fatalf("%s over a %q conversation: %v", door.name, status, err)
				}
				if minted != nil {
					t.Errorf("%s minted %s on a task that holds the un-ended %q conversation %s", door.name, minted.ID, status, held)
				}
				rows, err := store.ListForTask(ctx, orgID, taskID)
				if err != nil {
					t.Fatalf("ListForTask: %v", err)
				}
				if len(rows) != 1 || rows[0].ID != held {
					t.Errorf("conversations on the task after the refused mint = %d, want only %s", len(rows), held)
				}
			}

			// A second mint on a task the first one minted on is the same
			// refusal: the minted row is the un-ended one.
			taskID := newTask(t, seed, "mint-twice")
			first, err := door.mint(store, ctx, orgID, taskID, userID)
			if err != nil || first == nil {
				t.Fatalf("first %s = (%v, %v)", door.name, first, err)
			}
			second, err := door.mint(store, ctx, orgID, taskID, userID)
			if err != nil {
				t.Fatalf("second %s: %v", door.name, err)
			}
			if second != nil {
				t.Errorf("a second %s minted %s beside %s", door.name, second.ID, first.ID)
			}
		})
	}

	t.Run("HasRun_IsFalseUntilAnAssistantMessageExists", func(t *testing.T) {
		store, orgID, userID, seed := mk(t)
		ctx := context.Background()
		taskID := newTask(t, seed, "has-run")
		minted, err := store.MintForPersonClaim(ctx, orgID, taskID, userID)
		if err != nil || minted == nil {
			t.Fatalf("MintForPersonClaim = (%v, %v)", minted, err)
		}
		hasRun := func() bool {
			t.Helper()
			got, err := store.Get(ctx, orgID, minted.ID)
			if err != nil || got == nil {
				t.Fatalf("Get = (%v, %v)", got, err)
			}
			return got.HasRun
		}
		if hasRun() {
			t.Fatal("has_run = true before any message")
		}

		// A staged note is a user row, and a conversation nobody has typed
		// in collects them. Neither that nor a person's own message is a run.
		for _, subtype := range []string{domain.MessageSubtypeInjectionSystemNote, ""} {
			if _, err := store.InsertMessage(ctx, orgID, &domain.Message{
				ConversationID: minted.ID, Role: "user", Subtype: subtype, Content: "not a turn",
			}); err != nil {
				t.Fatalf("InsertMessage (user, subtype %q): %v", subtype, err)
			}
		}
		if hasRun() {
			t.Error("has_run = true on a conversation holding only user rows")
		}

		if _, err := store.InsertMessage(ctx, orgID, &domain.Message{
			ConversationID: minted.ID, Role: "assistant", Content: "a turn",
		}); err != nil {
			t.Fatalf("InsertMessage (assistant): %v", err)
		}
		if !hasRun() {
			t.Error("has_run = false after an assistant message")
		}
	})
}
