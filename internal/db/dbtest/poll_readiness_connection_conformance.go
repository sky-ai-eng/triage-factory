package dbtest

import (
	"context"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// PollReadinessStoreFactory hands the conformance suite a wired store and the
// orgID to scope every call. The org row must already exist, because
// ListConnectionStatuses reports only active orgs.
type PollReadinessStoreFactory func(t *testing.T) (store db.PollReadinessStore, orgID string)

// RunPollReadinessConnectionConformance covers the connection-status contract
// every backend impl must hold: an unrecorded pair reads as unknown; the write
// returns both the row it stored and the row it replaced; the time a state
// began moves only when the state changes; the failure class exists only while
// the connection is down; the list reports recorded states only; and the
// readiness columns sharing the row are independent of all of it.
func RunPollReadinessConnectionConformance(t *testing.T, mk PollReadinessStoreFactory) {
	t.Helper()
	ctx := context.Background()

	t.Run("unrecorded_pair_is_unknown", func(t *testing.T) {
		store, orgID := mk(t)
		got, err := store.Connection(ctx, orgID, "github")
		if err != nil {
			t.Fatalf("Connection: %v", err)
		}
		want := db.ConnectionStatus{OrgID: orgID, Source: "github", State: db.ConnectionUnknown}
		if got.OrgID != want.OrgID || got.Source != want.Source || got.State != want.State || got.ChangedAt != nil || got.FailureClass != "" {
			t.Errorf("Connection on an untouched pair = %+v, want %+v", got, want)
		}
		list, err := store.ListConnectionStatuses(ctx)
		if err != nil {
			t.Fatalf("ListConnectionStatuses: %v", err)
		}
		if len(list) != 0 {
			t.Errorf("ListConnectionStatuses on an untouched store = %+v, want empty", list)
		}
	})

	t.Run("RecordConnection_returns_the_stored_row_and_the_previous_one", func(t *testing.T) {
		store, orgID := mk(t)
		read := func() (*db.ConnectionStatus, error) {
			st, err := store.Connection(ctx, orgID, "jira")
			return &st, err
		}

		down, prev, err := store.RecordConnection(ctx, orgID, "jira", db.ConnectionDown, "transient")
		if err != nil {
			t.Fatalf("RecordConnection (insert): %v", err)
		}
		AssertWriteReturnedStoredRow(t, "RecordConnection (insert)", down, read)
		if prev.State != db.ConnectionUnknown || prev.ChangedAt != nil {
			t.Errorf("previous on the first record = %+v, want unknown with no ChangedAt", prev)
		}
		if down.State != db.ConnectionDown || down.FailureClass != "transient" || down.ChangedAt == nil {
			t.Errorf("stored on the first record = %+v, want down/transient with ChangedAt set", down)
		}

		up, prev, err := store.RecordConnection(ctx, orgID, "jira", db.ConnectionUp, "")
		if err != nil {
			t.Fatalf("RecordConnection (update): %v", err)
		}
		AssertWriteReturnedStoredRow(t, "RecordConnection (update)", up, read)
		if prev.State != db.ConnectionDown || prev.FailureClass != "transient" {
			t.Errorf("previous on the second record = %+v, want the down/transient row it replaced", prev)
		}
		if prev.ChangedAt == nil || !prev.ChangedAt.Equal(*down.ChangedAt) {
			t.Errorf("previous ChangedAt = %v, want the first record's %v", prev.ChangedAt, down.ChangedAt)
		}
	})

	t.Run("ChangedAt_moves_only_on_a_change_of_state", func(t *testing.T) {
		store, orgID := mk(t)
		first, _, err := store.RecordConnection(ctx, orgID, "github", db.ConnectionDown, "transient")
		if err != nil {
			t.Fatalf("RecordConnection: %v", err)
		}
		// The clocks both dialects stamp with are finer than this, so a state
		// change after it lands strictly later.
		time.Sleep(10 * time.Millisecond)

		// Still down, for a different reason: the outage began when it began.
		again, _, err := store.RecordConnection(ctx, orgID, "github", db.ConnectionDown, "auth")
		if err != nil {
			t.Fatalf("RecordConnection (still down): %v", err)
		}
		if again.ChangedAt == nil || !again.ChangedAt.Equal(*first.ChangedAt) {
			t.Errorf("ChangedAt after a second down = %v, want the first down's %v", again.ChangedAt, first.ChangedAt)
		}
		if again.FailureClass != "auth" {
			t.Errorf("FailureClass after a second down = %q, want the latest cycle's auth", again.FailureClass)
		}

		time.Sleep(10 * time.Millisecond)
		up, _, err := store.RecordConnection(ctx, orgID, "github", db.ConnectionUp, "")
		if err != nil {
			t.Fatalf("RecordConnection (up): %v", err)
		}
		if up.ChangedAt == nil || !up.ChangedAt.After(*first.ChangedAt) {
			t.Errorf("ChangedAt after going up = %v, want later than the down's %v", up.ChangedAt, first.ChangedAt)
		}
	})

	t.Run("FailureClass_is_cleared_whenever_the_state_is_not_down", func(t *testing.T) {
		store, orgID := mk(t)
		if _, _, err := store.RecordConnection(ctx, orgID, "github", db.ConnectionDown, "auth"); err != nil {
			t.Fatalf("RecordConnection (down): %v", err)
		}
		up, _, err := store.RecordConnection(ctx, orgID, "github", db.ConnectionUp, "")
		if err != nil {
			t.Fatalf("RecordConnection (up): %v", err)
		}
		if up.FailureClass != "" {
			t.Errorf("FailureClass after going up = %q, want cleared", up.FailureClass)
		}
		// A class handed in beside a state that is not down is not stored: an
		// up row carrying a failure class would be a claim nothing refuses.
		up, _, err = store.RecordConnection(ctx, orgID, "github", db.ConnectionUp, "transient")
		if err != nil {
			t.Fatalf("RecordConnection (up with a class): %v", err)
		}
		if up.FailureClass != "" {
			t.Errorf("FailureClass on an up row given a class = %q, want empty", up.FailureClass)
		}
	})

	t.Run("an_unknown_state_is_refused", func(t *testing.T) {
		store, orgID := mk(t)
		if _, _, err := store.RecordConnection(ctx, orgID, "github", db.ConnectionState("sideways"), ""); err == nil {
			t.Error("RecordConnection accepted a state outside the vocabulary")
		}
	})

	t.Run("ListConnectionStatuses_reports_recorded_states_only", func(t *testing.T) {
		store, orgID := mk(t)
		// A row the readiness columns created carries no connection fact.
		if err := store.MarkPollComplete(ctx, orgID, "slack", time.Time{}); err != nil {
			t.Fatalf("MarkPollComplete: %v", err)
		}
		if _, _, err := store.RecordConnection(ctx, orgID, "jira", db.ConnectionDown, "transient"); err != nil {
			t.Fatalf("RecordConnection jira: %v", err)
		}
		if _, _, err := store.RecordConnection(ctx, orgID, "github", db.ConnectionUp, ""); err != nil {
			t.Fatalf("RecordConnection github: %v", err)
		}
		list, err := store.ListConnectionStatuses(ctx)
		if err != nil {
			t.Fatalf("ListConnectionStatuses: %v", err)
		}
		if len(list) != 2 {
			t.Fatalf("ListConnectionStatuses = %+v, want github and jira only", list)
		}
		if list[0].Source != "github" || list[0].State != db.ConnectionUp {
			t.Errorf("list[0] = %+v, want github up", list[0])
		}
		if list[1].Source != "jira" || list[1].State != db.ConnectionDown || list[1].FailureClass != "transient" {
			t.Errorf("list[1] = %+v, want jira down/transient", list[1])
		}
		for _, st := range list {
			want, err := store.Connection(ctx, orgID, st.Source)
			if err != nil {
				t.Fatalf("Connection %s: %v", st.Source, err)
			}
			AssertWriteReturnedStoredRow(t, "ListConnectionStatuses "+st.Source, st, func() (*db.ConnectionStatus, error) { return &want, nil })
		}
	})

	t.Run("readiness_and_connection_are_independent", func(t *testing.T) {
		store, orgID := mk(t)
		if err := store.MarkPollComplete(ctx, orgID, "jira", time.Time{}); err != nil {
			t.Fatalf("MarkPollComplete: %v", err)
		}
		if _, _, err := store.RecordConnection(ctx, orgID, "jira", db.ConnectionDown, "transient"); err != nil {
			t.Fatalf("RecordConnection: %v", err)
		}
		if ready, err := store.Ready(ctx, orgID, "jira"); err != nil || !ready {
			t.Errorf("Ready after recording a connection = %v, %v; want true — the connection write must not touch readiness", ready, err)
		}
		if err := store.MarkRestarted(ctx, orgID, "jira"); err != nil {
			t.Fatalf("MarkRestarted: %v", err)
		}
		st, err := store.Connection(ctx, orgID, "jira")
		if err != nil {
			t.Fatalf("Connection: %v", err)
		}
		if st.State != db.ConnectionDown || st.FailureClass != "transient" {
			t.Errorf("Connection after a restart = %+v, want the recorded down/transient kept", st)
		}
	})
}
