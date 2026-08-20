package dbtest

import (
	"context"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// PermissionStoreFactory is what a per-backend test file hands to
// RunPermissionStoreConformance. Returns:
//   - the wired PermissionStore impl,
//   - the orgID and a real userID (the decided_by FK target) to pass to calls,
//   - a PermissionSeeder staging the conversation/claim FK chain the store
//     doesn't own.
type PermissionStoreFactory func(t *testing.T) (store db.PermissionStore, orgID, userID string, seed PermissionSeeder)

// PermissionSeeder is the bag of callbacks the suite uses for rows
// PermissionStore doesn't own, plus the raw audit-column readback.
type PermissionSeeder struct {
	// Conversation inserts a conversation row and returns its id.
	Conversation func(t *testing.T) (conversationID string)

	// Claim mints an ACTIVE claim (released_at NULL) on the conversation and
	// returns its id. At most one active claim per conversation is
	// schema-enforced, so a second one needs the first released.
	Claim func(t *testing.T, conversationID string) (claimID string)

	// ReleaseClaim stamps released_at on a claim — the event that makes every
	// prompt it asked stop being pending.
	ReleaseClaim func(t *testing.T, claimID string)

	// Load reads one row's audit columns straight out of the table. The store's
	// own Get answers a full-row point read (added for the resolve endpoint's
	// cross-pod fallback — see the interface doc), so this exists only for the
	// narrower audit-column assertions (reason, decided_by, waited_ms) that
	// would otherwise mean unpacking a whole domain.ConversationPermission
	// just to check five fields. found=false when no row matches.
	Load func(t *testing.T, conversationID, toolCallID string) (row PermissionRow, found bool)
}

// PermissionRow is the raw audit view a PermissionSeeder.Load returns.
type PermissionRow struct {
	State        string
	Reason       string
	DecidedBy    string
	HasDecidedAt bool
	WaitedMs     *int
}

// RunPermissionStoreConformance covers the contract every backend must hold,
// including the returned-row standard on Create and Resolve
// (create_returns_the_stored_row, resolve_returns_the_stored_row — both
// asserted against Get, the point read added for exactly this: no other
// PermissionStore method finds a row regardless of state) and Resolve's
// declined-guard outcome (pinned inside first_resolution_wins and
// resolving_an_unknown_call_is_not_an_error: a racing or missing resolution
// is (nil, nil), never an error). get_returns_a_row_regardless_of_state
// covers Get itself, pending and resolved alike.
//
// Unlike JiraAppsStore.UpsertForOrg, there is no separate Postgres-app-pool
// arm for these writes: Create and Resolve are unconditionally admin-pool in
// both dialects (see the interface doc) with no app-pool-doored twin RLS
// could gate, so running "the write" under real claims would test nothing
// the admin-pool run here doesn't already cover. This suite's own Postgres
// wiring runs everything — writes and the pending read alike — on AdminDB
// (RLS-bypassed) for that reason.
//
// The through-line of every case here is that a prompt has to be READABLE by
// someone who never saw the websocket frame, and a decision has to be
// distinguishable from a non-decision without parsing prose. So: a created
// prompt is listable, a resolved one is not, a prompt whose engagement has gone
// away is not pending EVEN IF nothing ever wrote its expiry, and every terminal
// path leaves a row whose reason says which path it was.
func RunPermissionStoreConformance(t *testing.T, mk PermissionStoreFactory) {
	t.Helper()
	ctx := context.Background()

	// newPending is the fixture every case starts from: a conversation with a
	// live claim, and one pending prompt on it.
	newPending := func(t *testing.T, store db.PermissionStore, orgID string, seed PermissionSeeder, toolCallID string, requestedAt time.Time) (conversationID, claimID string) {
		t.Helper()
		conversationID = seed.Conversation(t)
		claimID = seed.Claim(t, conversationID)
		expires := requestedAt.Add(2 * time.Minute)
		created, err := store.Create(ctx, orgID, domain.ConversationPermission{
			ConversationID: conversationID,
			ClaimID:        claimID,
			ToolCallID:     toolCallID,
			ToolName:       "Bash",
			Input:          map[string]any{"command": "rm -rf ./build"},
			Title:          "Claude wants to run rm -rf ./build",
			State:          domain.PermissionStatePending,
			RequestedAt:    requestedAt,
			ExpiresAt:      &expires,
		})
		if err != nil {
			t.Fatalf("create pending permission: %v", err)
		}
		if created.ID == "" {
			t.Fatal("Create must return a row with an id — the caller and the row have to agree on identity")
		}
		return conversationID, claimID
	}

	t.Run("created_prompt_is_listable_with_everything_needed_to_answer_it", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		requestedAt := time.Now().UTC().Add(-3 * time.Second)
		conversationID, _ := newPending(t, store, orgID, seed, "toolu_01", requestedAt)

		got, err := store.ListPending(ctx, orgID, conversationID)
		if err != nil {
			t.Fatalf("list pending: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("expected 1 pending prompt, got %d: %+v", len(got), got)
		}
		p := got[0]
		if p.ToolCallID != "toolu_01" || p.ToolName != "Bash" {
			t.Fatalf("prompt identity lost: %+v", p)
		}
		// The input is what the user actually approves — a surface that can't
		// read it back can only show the SDK's sentence, which is not the same
		// thing.
		if cmd, _ := p.Input["command"].(string); cmd != "rm -rf ./build" {
			t.Fatalf("tool input lost: %+v", p.Input)
		}
		if p.Title != "Claude wants to run rm -rf ./build" {
			t.Fatalf("title lost: %q", p.Title)
		}
		if p.ExpiresAt == nil {
			t.Fatal("expires_at lost — a reconstructed prompt needs the deadline it actually has left")
		}
		if p.State != domain.PermissionStatePending || p.Reason != "" || p.DecidedBy != "" || p.WaitedMs != nil {
			t.Fatalf("a pending row must carry no verdict: %+v", p)
		}
	})

	t.Run("create_returns_the_stored_row", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		conversationID := seed.Conversation(t)
		claimID := seed.Claim(t, conversationID)
		requestedAt := time.Now().UTC().Add(-time.Second)
		expires := requestedAt.Add(2 * time.Minute)

		created, err := store.Create(ctx, orgID, domain.ConversationPermission{
			ConversationID: conversationID,
			ClaimID:        claimID,
			ToolCallID:     "toolu_returned_row",
			ToolName:       "Bash",
			Input:          map[string]any{"command": "echo hi"},
			Title:          "Claude wants to run echo hi",
			State:          domain.PermissionStatePending,
			RequestedAt:    requestedAt,
			ExpiresAt:      &expires,
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "Create", created, func() (*domain.ConversationPermission, error) {
			return store.Get(ctx, orgID, conversationID, "toolu_returned_row")
		})
	})

	t.Run("second_row_for_same_call_is_rejected", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		now := time.Now().UTC()
		conversationID, claimID := newPending(t, store, orgID, seed, "toolu_dup", now)

		_, err := store.Create(ctx, orgID, domain.ConversationPermission{
			ConversationID: conversationID,
			ClaimID:        claimID,
			ToolCallID:     "toolu_dup",
			ToolName:       "Bash",
			State:          domain.PermissionStatePending,
			RequestedAt:    now,
		})
		if err == nil {
			t.Fatal("a second row for the same (conversation, tool_call_id) must be refused — one row per gated call is what ties a decision to the call it authorized")
		}
	})

	t.Run("resolve_returns_the_stored_row", func(t *testing.T) {
		store, orgID, userID, seed := mk(t)
		requestedAt := time.Now().UTC().Add(-4 * time.Second)
		conversationID, _ := newPending(t, store, orgID, seed, "toolu_resolved_row", requestedAt)

		resolved, err := store.Resolve(ctx, orgID, conversationID, "toolu_resolved_row",
			domain.PermissionStateAllowed, domain.PermissionReasonUser, userID)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if resolved == nil {
			t.Fatal("Resolve returned nil for a genuinely pending prompt")
		}
		AssertWriteReturnedStoredRow(t, "Resolve", *resolved, func() (*domain.ConversationPermission, error) {
			return store.Get(ctx, orgID, conversationID, "toolu_resolved_row")
		})
	})

	t.Run("get_returns_a_row_regardless_of_state", func(t *testing.T) {
		store, orgID, userID, seed := mk(t)
		conversationID, _ := newPending(t, store, orgID, seed, "toolu_get", time.Now().UTC())

		got, err := store.Get(ctx, orgID, conversationID, "toolu_get")
		if err != nil {
			t.Fatalf("Get (pending): %v", err)
		}
		if got == nil || got.State != domain.PermissionStatePending {
			t.Fatalf("Get (pending) = %+v, want the pending row", got)
		}

		if _, err := store.Resolve(ctx, orgID, conversationID, "toolu_get",
			domain.PermissionStateAllowed, domain.PermissionReasonUser, userID); err != nil {
			t.Fatalf("resolve: %v", err)
		}
		// Unlike ListPending, Get still finds a resolved prompt — the resolve
		// endpoint's cross-pod fallback needs exactly this, since by the time
		// it reads, the prompt is no longer pending.
		got, err = store.Get(ctx, orgID, conversationID, "toolu_get")
		if err != nil {
			t.Fatalf("Get (resolved): %v", err)
		}
		if got == nil || got.State != domain.PermissionStateAllowed {
			t.Fatalf("Get (resolved) = %+v, want the allowed row", got)
		}

		if got, err := store.Get(ctx, orgID, conversationID, "toolu_get_never_existed"); err != nil || got != nil {
			t.Fatalf("Get on an unknown tool call = %+v (err %v), want (nil, nil)", got, err)
		}
	})

	t.Run("approve_records_who_allowed_it_and_how_long_they_took", func(t *testing.T) {
		store, orgID, userID, seed := mk(t)
		// Backdated so the stamped wait is a real, assertable number rather
		// than a rounding artifact.
		requestedAt := time.Now().UTC().Add(-4 * time.Second)
		conversationID, _ := newPending(t, store, orgID, seed, "toolu_allow", requestedAt)

		if _, err := store.Resolve(ctx, orgID, conversationID, "toolu_allow",
			domain.PermissionStateAllowed, domain.PermissionReasonUser, userID); err != nil {
			t.Fatalf("resolve allow: %v", err)
		}

		row, found := seed.Load(t, conversationID, "toolu_allow")
		if !found {
			t.Fatal("the row must survive its resolution — an approval that leaves no trace is the gap this table closes")
		}
		if row.State != domain.PermissionStateAllowed || row.Reason != domain.PermissionReasonUser {
			t.Fatalf("state/reason = %q/%q, want allowed/user", row.State, row.Reason)
		}
		if row.DecidedBy != userID {
			t.Fatalf("decided_by = %q, want %q — an approval has to name who granted it", row.DecidedBy, userID)
		}
		if !row.HasDecidedAt {
			t.Fatal("decided_at must be stamped")
		}
		if row.WaitedMs == nil {
			t.Fatal("waited_ms must be stamped at resolution — how long a human was left waiting is not recoverable later")
		}
		if *row.WaitedMs < 3000 || *row.WaitedMs > 120_000 {
			t.Fatalf("waited_ms = %d, want ~4000 (measured from the stored requested_at, not from now)", *row.WaitedMs)
		}

		if got, err := store.ListPending(ctx, orgID, conversationID); err != nil || len(got) != 0 {
			t.Fatalf("an answered prompt must leave the pending set: %+v (err %v)", got, err)
		}
	})

	t.Run("every_deny_path_is_distinguishable_by_reason_alone", func(t *testing.T) {
		store, orgID, userID, seed := mk(t)
		// The four ways a prompt can end without being allowed. The agent sees
		// prose for all of them and the prose is free to change; these enum
		// values are what anything else has to read.
		cases := []struct {
			name      string
			toolCall  string
			state     string
			reason    string
			decidedBy string
		}{
			{"a human denied it", "toolu_user_deny", domain.PermissionStateDenied, domain.PermissionReasonUser, userID},
			{"watched but unanswered", "toolu_timeout", domain.PermissionStateDenied, domain.PermissionReasonTimeout, ""},
			{"nobody was watching", "toolu_absent", domain.PermissionStateDenied, domain.PermissionReasonAbsent, ""},
			{"the engagement went away", "toolu_lost", domain.PermissionStateExpired, domain.PermissionReasonClaimLost, ""},
		}
		for _, tc := range cases {
			conversationID, _ := newPending(t, store, orgID, seed, tc.toolCall, time.Now().UTC().Add(-time.Second))
			if _, err := store.Resolve(ctx, orgID, conversationID, tc.toolCall, tc.state, tc.reason, tc.decidedBy); err != nil {
				t.Fatalf("%s: resolve: %v", tc.name, err)
			}
			row, found := seed.Load(t, conversationID, tc.toolCall)
			if !found {
				t.Fatalf("%s: row vanished", tc.name)
			}
			if row.State != tc.state || row.Reason != tc.reason {
				t.Fatalf("%s: state/reason = %q/%q, want %q/%q", tc.name, row.State, row.Reason, tc.state, tc.reason)
			}
			if tc.decidedBy == "" && row.DecidedBy != "" {
				t.Fatalf("%s: decided_by = %q, want empty — no human decided this", tc.name, row.DecidedBy)
			}
			if row.WaitedMs == nil {
				t.Fatalf("%s: waited_ms must be stamped on every terminal, not just approvals", tc.name)
			}
		}
	})

	t.Run("first_resolution_wins", func(t *testing.T) {
		store, orgID, userID, seed := mk(t)
		conversationID, _ := newPending(t, store, orgID, seed, "toolu_race", time.Now().UTC().Add(-time.Second))

		first, err := store.Resolve(ctx, orgID, conversationID, "toolu_race",
			domain.PermissionStateAllowed, domain.PermissionReasonUser, userID)
		if err != nil {
			t.Fatalf("first resolve: %v", err)
		}
		if first == nil || first.State != domain.PermissionStateAllowed {
			t.Fatalf("first resolve = %+v, want the allowed row", first)
		}
		// The broker's deregister-then-drain can produce a second attempt for a
		// prompt a deadline and a click both reached. It must not rewrite the
		// verdict that already went to the agent — the guard declines, which is
		// (nil, nil), not an error.
		again, err := store.Resolve(ctx, orgID, conversationID, "toolu_race",
			domain.PermissionStateDenied, domain.PermissionReasonTimeout, "")
		if err != nil {
			t.Fatalf("second resolve must be a silent no-op, got %v", err)
		}
		if again != nil {
			t.Fatalf("second resolve = %+v, want nil — the guard declining", again)
		}
		row, _ := seed.Load(t, conversationID, "toolu_race")
		if row.State != domain.PermissionStateAllowed || row.Reason != domain.PermissionReasonUser || row.DecidedBy != userID {
			t.Fatalf("a settled verdict was overwritten: %+v", row)
		}
	})

	t.Run("resolving_an_unknown_call_is_not_an_error", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		conversationID := seed.Conversation(t)
		// Recording is best-effort alongside a broker that has already decided;
		// a missing row must not turn into a failure on that path, and the
		// return is the same (nil, nil) shape as a racing second resolution.
		got, err := store.Resolve(ctx, orgID, conversationID, "toolu_never_existed",
			domain.PermissionStateDenied, domain.PermissionReasonTimeout, "")
		if err != nil {
			t.Fatalf("resolving a prompt that was never recorded must be a no-op, got %v", err)
		}
		if got != nil {
			t.Fatalf("resolving a prompt that was never recorded = %+v, want nil", got)
		}
	})

	t.Run("a_released_claims_prompt_is_not_pending_even_if_nothing_expired_it", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		conversationID, claimID := newPending(t, store, orgID, seed, "toolu_orphan", time.Now().UTC())

		// Release WITHOUT calling ExpireForClaim: this is the crash case, where
		// the process that asked the question died and ran no cleanup at all.
		// The row still says 'pending' and must still read as not pending.
		seed.ReleaseClaim(t, claimID)

		got, err := store.ListPending(ctx, orgID, conversationID)
		if err != nil {
			t.Fatalf("list pending: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("a prompt asked by a released engagement must not be pending: %+v", got)
		}
		if row, found := seed.Load(t, conversationID, "toolu_orphan"); !found || row.State != domain.PermissionStatePending {
			t.Fatalf("the stored row must be untouched (nothing ran) — got found=%v %+v", found, row)
		}

		// A successor engagement's own prompt IS pending: the filter is
		// "the conversation's current claim", not "any claim".
		successor := seed.Claim(t, conversationID)
		if _, err := store.Create(ctx, orgID, domain.ConversationPermission{
			ConversationID: conversationID,
			ClaimID:        successor,
			ToolCallID:     "toolu_successor",
			ToolName:       "Read",
			State:          domain.PermissionStatePending,
		}); err != nil {
			t.Fatalf("create successor prompt: %v", err)
		}
		got, err = store.ListPending(ctx, orgID, conversationID)
		if err != nil {
			t.Fatalf("list pending after successor: %v", err)
		}
		if len(got) != 1 || got[0].ToolCallID != "toolu_successor" {
			t.Fatalf("expected only the live engagement's prompt, got %+v", got)
		}
	})

	t.Run("expire_for_claim_settles_the_rows_it_left_behind", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		conversationID, claimID := newPending(t, store, orgID, seed, "toolu_expire", time.Now().UTC().Add(-2*time.Second))

		if err := store.ExpireForClaim(ctx, orgID, claimID); err != nil {
			t.Fatalf("expire for claim: %v", err)
		}
		row, found := seed.Load(t, conversationID, "toolu_expire")
		if !found {
			t.Fatal("row vanished")
		}
		if row.State != domain.PermissionStateExpired || row.Reason != domain.PermissionReasonClaimLost {
			t.Fatalf("state/reason = %q/%q, want expired/claim_lost", row.State, row.Reason)
		}
		if row.WaitedMs == nil || *row.WaitedMs < 1000 {
			t.Fatalf("waited_ms = %v, want the ~2s the prompt actually stood open", row.WaitedMs)
		}
		// Idempotent: only pending rows are touched, so a retry can't restamp
		// a settled one.
		if err := store.ExpireForClaim(ctx, orgID, claimID); err != nil {
			t.Fatalf("second expire: %v", err)
		}
	})

	t.Run("expire_for_claim_leaves_other_engagements_alone", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		convA, claimA := newPending(t, store, orgID, seed, "toolu_a", time.Now().UTC())
		convB, _ := newPending(t, store, orgID, seed, "toolu_b", time.Now().UTC())

		if err := store.ExpireForClaim(ctx, orgID, claimA); err != nil {
			t.Fatalf("expire for claim: %v", err)
		}
		if got, _ := store.ListPending(ctx, orgID, convA); len(got) != 0 {
			t.Fatalf("conversation A should have nothing pending: %+v", got)
		}
		got, err := store.ListPending(ctx, orgID, convB)
		if err != nil {
			t.Fatalf("list pending B: %v", err)
		}
		if len(got) != 1 || got[0].ToolCallID != "toolu_b" {
			t.Fatalf("another engagement's prompt was collateral damage: %+v", got)
		}
		if err := store.ExpireForClaim(ctx, orgID, ""); err != nil {
			t.Fatalf("an empty claim id must be a no-op, got %v", err)
		}
		if got, _ := store.ListPending(ctx, orgID, convB); len(got) != 1 {
			t.Fatalf("an empty claim id must not sweep everything: %+v", got)
		}
	})

	t.Run("pending_set_is_per_conversation_and_oldest_first", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		base := time.Now().UTC().Add(-time.Minute)
		conversationID := seed.Conversation(t)
		claimID := seed.Claim(t, conversationID)
		// Written newest-first so ordering can't pass by insertion accident.
		for i, off := range []time.Duration{2 * time.Second, 0, time.Second} {
			if _, err := store.Create(ctx, orgID, domain.ConversationPermission{
				ConversationID: conversationID,
				ClaimID:        claimID,
				ToolCallID:     []string{"toolu_third", "toolu_first", "toolu_second"}[i],
				ToolName:       "Read",
				State:          domain.PermissionStatePending,
				RequestedAt:    base.Add(off),
			}); err != nil {
				t.Fatalf("create prompt %d: %v", i, err)
			}
		}
		other := seed.Conversation(t)
		otherClaim := seed.Claim(t, other)
		if _, err := store.Create(ctx, orgID, domain.ConversationPermission{
			ConversationID: other, ClaimID: otherClaim, ToolCallID: "toolu_elsewhere",
			ToolName: "Read", State: domain.PermissionStatePending, RequestedAt: base,
		}); err != nil {
			t.Fatalf("create other conversation's prompt: %v", err)
		}

		got, err := store.ListPending(ctx, orgID, conversationID)
		if err != nil {
			t.Fatalf("list pending: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("expected 3 prompts, got %d: %+v", len(got), got)
		}
		// Parallel tool calls in one assistant message each get their own
		// prompt, and the user answers them in the order they were asked.
		for i, want := range []string{"toolu_first", "toolu_second", "toolu_third"} {
			if got[i].ToolCallID != want {
				t.Fatalf("prompt %d = %q, want %q (pending set must be oldest-first)", i, got[i].ToolCallID, want)
			}
		}
	})

	t.Run("conversation_with_no_claim_has_nothing_pending", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		conversationID := seed.Conversation(t)
		got, err := store.ListPending(ctx, orgID, conversationID)
		if err != nil {
			t.Fatalf("list pending on a claimless conversation must not error: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("expected no prompts, got %+v", got)
		}
	})

	t.Run("create_refuses_a_non_pending_state", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		conversationID := seed.Conversation(t)
		claimID := seed.Claim(t, conversationID)
		// A prompt is born pending. Minting one already-decided would mean a
		// decision nobody made.
		if _, err := store.Create(ctx, orgID, domain.ConversationPermission{
			ConversationID: conversationID, ClaimID: claimID, ToolCallID: "toolu_prebaked",
			ToolName: "Read", State: domain.PermissionStateAllowed,
		}); err == nil {
			t.Fatal("Create must refuse a non-pending state")
		}
	})

	t.Run("create_refuses_a_prompt_with_no_claim", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		conversationID := seed.Conversation(t)
		seed.Claim(t, conversationID)

		// A row with no claim is the worst shape this table can hold: invisible
		// to ListPending (which matches on the active claim, and NULL matches
		// nothing) and untouched by ExpireForClaim (which keys on claim_id), so
		// it would sit at 'pending' forever with nothing able to surface it or
		// settle it. Refusing the write is strictly better than accepting one —
		// the caller records nothing, logs, and still asks the question.
		_, err := store.Create(ctx, orgID, domain.ConversationPermission{
			ConversationID: conversationID,
			ToolCallID:     "toolu_unclaimed",
			ToolName:       "Bash",
			State:          domain.PermissionStatePending,
		})
		if err == nil {
			t.Fatal("Create must refuse a pending prompt with no claim — it would be permanently unreadable and unsettleable")
		}
		if got, err := store.ListPending(ctx, orgID, conversationID); err != nil || len(got) != 0 {
			t.Fatalf("nothing should have been written: %+v (err %v)", got, err)
		}
	})

	t.Run("resolve_refuses_values_outside_the_vocabulary", func(t *testing.T) {
		store, orgID, _, seed := mk(t)
		conversationID, _ := newPending(t, store, orgID, seed, "toolu_vocab", time.Now().UTC())

		// The enum is the whole point of the column: a typo'd reason silently
		// stored is a distinction lost, which is the state this table exists to
		// get out of.
		if _, err := store.Resolve(ctx, orgID, conversationID, "toolu_vocab",
			domain.PermissionStateDenied, "gave_up", ""); err == nil {
			t.Fatal("Resolve must refuse an unknown reason")
		}
		if _, err := store.Resolve(ctx, orgID, conversationID, "toolu_vocab", "maybe", domain.PermissionReasonUser, ""); err == nil {
			t.Fatal("Resolve must refuse an unknown state")
		}
		if _, err := store.Resolve(ctx, orgID, conversationID, "toolu_vocab",
			domain.PermissionStatePending, "", ""); err == nil {
			t.Fatal("Resolve must refuse 'pending' — it stamps terminals, not the state the row was born in")
		}
		if got, _ := store.ListPending(ctx, orgID, conversationID); len(got) != 1 {
			t.Fatalf("a refused resolve must leave the prompt pending: %+v", got)
		}
	})
}
