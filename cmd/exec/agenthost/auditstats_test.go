package agenthost

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/sidecarproto"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// installTestAuditDrops points the package's drop instrument at an in-memory
// ManualReader for the duration of one test, restoring the process-global
// afterwards. Swapping the var (rather than threading the stats through four
// call sites in two placements) is what keeps the instrumentation free at the
// call site; the restore is what keeps tests independent.
func installTestAuditDrops(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	prev := auditDrops
	auditDrops = newAuditDropStats(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { auditDrops = prev })
	return reader
}

// collectDrops reads audit.records.dropped back as {stage: {op: count}} — the
// two-dimensional shape the dashboard reads, so a test asserts on the same
// thing an operator sees. Absent metric → empty map.
func collectDrops(t *testing.T, reader *sdkmetric.ManualReader) map[string]map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	out := map[string]map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "audit.records.dropped" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("audit.records.dropped: data is %T, want Sum[int64]", m.Data)
			}
			for _, dp := range sum.DataPoints {
				stage, ok := dp.Attributes.Value(attribute.Key("stage"))
				if !ok {
					t.Fatalf("datapoint missing stage (attrs: %v)", dp.Attributes.Encoded(attribute.DefaultEncoder()))
				}
				op, ok := dp.Attributes.Value(attribute.Key("op"))
				if !ok {
					t.Fatalf("datapoint missing op (attrs: %v)", dp.Attributes.Encoded(attribute.DefaultEncoder()))
				}
				if out[stage.AsString()] == nil {
					out[stage.AsString()] = map[string]int64{}
				}
				out[stage.AsString()][op.AsString()] += dp.Value
			}
		}
	}
	return out
}

// TestAuditDrop_RecordFailureIsCounted is the write-stage half of the
// acceptance: a forced DB failure inside the write funnel — the swallow that
// used to leave nothing but a warn line — increments the counter, naming the
// row it lost.
func TestAuditDrop_RecordFailureIsCounted(t *testing.T) {
	reader := installTestAuditDrops(t)
	conn, stores, info := newCaptureStoresConn(t, true)
	// Kill the store out from under the funnel. Every write below now errors,
	// which is the only way to reach the swallow without a fake store.
	if err := conn.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	RecordExternalWrite(context.Background(), stores, info, nil, &domain.ExternalAction{
		Provider:   domain.ProviderGitHub,
		Action:     domain.ActionGHChannelWrite,
		Target:     "acme/widgets",
		Credential: domain.CredentialGitHubApp,
	})

	got := collectDrops(t, reader)
	if n := got[dropStageRecord][domain.ActionGHChannelWrite]; n != 1 {
		t.Fatalf("drops = %v; want one %s drop under op=%s", got, dropStageRecord, domain.ActionGHChannelWrite)
	}
}

// TestAuditDrop_RecordFailureNamesTheDeliverableKind: a deliverable-bearing write
// is named by its kind rather than the action's, matching the attribute the
// artifact.record span at the same site already sets.
func TestAuditDrop_RecordFailureNamesTheDeliverableKind(t *testing.T) {
	reader := installTestAuditDrops(t)
	conn, stores, info := newCaptureStoresConn(t, true)
	if err := conn.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	RecordExternalWrite(context.Background(), stores, info,
		&domain.Deliverable{Provider: domain.ProviderGitHub, Kind: domain.DeliverableKindPullRequest, Target: "acme/widgets#7"},
		&domain.ExternalAction{Provider: domain.ProviderGitHub, Action: domain.ActionPRCreated, Target: "acme/widgets#7"})

	got := collectDrops(t, reader)
	if n := got[dropStageRecord][domain.DeliverableKindPullRequest]; n != 1 {
		t.Fatalf("drops = %v; want one %s drop under op=%s", got, dropStageRecord, domain.DeliverableKindPullRequest)
	}
}

// TestAuditDrop_SuccessfulWriteCountsNothing: the counter must stay at zero on
// the path every run takes, or "did we lose anything" has no answer.
func TestAuditDrop_SuccessfulWriteCountsNothing(t *testing.T) {
	reader := installTestAuditDrops(t)
	stores, info := newCaptureStores(t, true)

	RecordExternalWrite(context.Background(), stores, info, nil, &domain.ExternalAction{
		Provider:   domain.ProviderGitHub,
		Action:     domain.ActionGHChannelWrite,
		Target:     "acme/widgets",
		Credential: domain.CredentialGitHubApp,
	})

	if acts := listExternalActions(t, stores); len(acts) != 1 {
		t.Fatalf("want the row actually written, got %d", len(acts))
	}
	if got := collectDrops(t, reader); len(got) != 0 {
		t.Fatalf("drops = %v; want none on a write that landed", got)
	}
}

// TestAuditDrop_RelayDecodeFailureIsCounted is the relay half of the
// acceptance: a record whose payload never becomes a write is counted at the
// decode stage, naming the op it arrived under — and acknowledged, since the
// same bytes would fail the same way on every resend.
func TestAuditDrop_RelayDecodeFailureIsCounted(t *testing.T) {
	reader := installTestAuditDrops(t)
	stores, info := newCaptureStores(t, true)
	s := NewRelayServer(stores, info, nil)

	// Well-formed JSON, wrong shape for this op — the failure a version skew or
	// a corrupted frame produces, rather than a syntactically broken one.
	if err := relayRecord(t, s, agentproc.RelayNamespaceCore, agentproc.OpRecordGHWrite,
		json.RawMessage(`{"status":"not-an-int"}`)); err != nil {
		t.Fatalf("an undecodable record was answered %v; want it acknowledged, so the sidecar does not resend it", err)
	}

	if acts := listExternalActions(t, stores); len(acts) != 0 {
		t.Fatalf("an undecodable record wrote %d rows; want none", len(acts))
	}
	got := collectDrops(t, reader)
	wantOp := agentproc.RelayNamespaceCore + "." + agentproc.OpRecordGHWrite
	if n := got[dropStageRelayDecode][wantOp]; n != 1 {
		t.Fatalf("drops = %v; want one %s drop under op=%s", got, dropStageRelayDecode, wantOp)
	}
}

// TestAuditDrop_RecordWithoutAKeyIsCounted: the key is what makes applying a
// record twice safe, so a record without a usable one is not applied at all.
// It is counted at the decode stage and acknowledged.
func TestAuditDrop_RecordWithoutAKeyIsCounted(t *testing.T) {
	reader := installTestAuditDrops(t)
	stores, info := newCaptureStores(t, true)
	s := NewRelayServer(stores, info, nil)

	args, _ := json.Marshal(agentproc.RecordGHWriteArgs{Method: "PUT", Path: "/repos/acme/widgets/pulls/7/merge", Status: 404})
	if err := s.DispatchRecord(context.Background(), agentproc.RelayNamespaceCore, agentproc.OpRecordGHWrite, "not-a-key", args); err != nil {
		t.Fatalf("DispatchRecord = %v; want the keyless record acknowledged", err)
	}
	if acts := listExternalActions(t, stores); len(acts) != 0 {
		t.Fatalf("a keyless record wrote %d rows; want none", len(acts))
	}
	wantOp := agentproc.RelayNamespaceCore + "." + agentproc.OpRecordGHWrite
	if n := collectDrops(t, reader)[dropStageRelayDecode][wantOp]; n != 1 {
		t.Fatalf("want one %s drop under op=%s", dropStageRelayDecode, wantOp)
	}
}

// TestAuditDrop_UnsupportedRecordOpIsCounted: an op with no case here lost a
// record just as surely as an undecodable one, and its name came off the wire,
// so it is counted under the clamp rather than verbatim.
func TestAuditDrop_UnsupportedRecordOpIsCounted(t *testing.T) {
	reader := installTestAuditDrops(t)
	s := NewRelayServer(db.Stores{}, ConversationInfo{}, nil)

	if err := relayRecord(t, s, agentproc.RelayNamespaceCore, "record_something_invented", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("an unsupported record was answered %v; want it acknowledged", err)
	}

	got := collectDrops(t, reader)
	if n := got[dropStageRelayDecode][dropOpOther]; n != 1 {
		t.Fatalf("drops = %v; want one %s drop under op=%s", got, dropStageRelayDecode, dropOpOther)
	}
}

// TestAuditDrop_UndeliveredRecordsAreCountedAtTheSendStage: what the sidecar's
// end-of-run drain reports it never delivered lands at the SEND stage, by op
// and count — none of it was ever applied here.
func TestAuditDrop_UndeliveredRecordsAreCountedAtTheSendStage(t *testing.T) {
	reader := installTestAuditDrops(t)

	CountUndeliveredRecords(context.Background(), "conv", []sidecarproto.UndeliveredRecords{
		{Namespace: agentproc.RelayNamespaceCore, Op: agentproc.OpRecordGHWrite, Count: 3},
		{Namespace: agentproc.RelayNamespaceCore, Op: agentproc.OpRecordPush, Count: 1},
		// Not a count anything can be: ignored rather than subtracted.
		{Namespace: agentproc.RelayNamespaceCore, Op: agentproc.OpRecordDenial, Count: -4},
	})

	got := collectDrops(t, reader)
	if n := got[dropStageRelaySend][agentproc.RelayNamespaceCore+"."+agentproc.OpRecordGHWrite]; n != 3 {
		t.Errorf("drops = %v; want three %s drops of the gh write", got, dropStageRelaySend)
	}
	if n := got[dropStageRelaySend][agentproc.RelayNamespaceCore+"."+agentproc.OpRecordPush]; n != 1 {
		t.Errorf("drops = %v; want one %s drop of the push", got, dropStageRelaySend)
	}
	if _, counted := got[dropStageRelaySend][agentproc.RelayNamespaceCore+"."+agentproc.OpRecordDenial]; counted {
		t.Errorf("drops = %v; a negative count must not be counted", got)
	}
}

// TestAuditDrop_WireOpNamesAreClamped: the op label arrives from the process
// deliberately exposed to hostile text, so anything outside the known set
// collapses to a single series. Unclamped, naming ops that do not exist would
// be an unbounded-cardinality write into the orchestrator's exporter.
func TestAuditDrop_WireOpNamesAreClamped(t *testing.T) {
	reader := installTestAuditDrops(t)

	CountUndeliveredRecords(context.Background(), "conv", []sidecarproto.UndeliveredRecords{
		{Namespace: agentproc.RelayNamespaceCore, Op: "invented_one", Count: 1},
		{Namespace: agentproc.RelayNamespaceCore, Op: "invented_two", Count: 1},
		// An unregistered namespace: nothing serves it, so there is no name to
		// vouch for.
		{Namespace: "not_a_provider", Op: agentproc.OpRecordGHWrite, Count: 1},
	})

	got := collectDrops(t, reader)
	if len(got[dropStageRelaySend]) != 1 || got[dropStageRelaySend][dropOpOther] != 3 {
		t.Fatalf("drops = %v; want all three collapsed onto op=%s", got, dropOpOther)
	}
}

// TestAuditDrop_RegisteredProviderOpIsNamed: the clamp is against what this
// binary REGISTERS, not against the core catalog — so a provider's own audit op
// keeps its name. Without this, every provider drop would land on "other" and
// the by-op panel could never break down the relay_dispatch stage, which only
// ever fires for provider ops.
func TestAuditDrop_RegisteredProviderOpIsNamed(t *testing.T) {
	reader := installTestAuditDrops(t)
	t.Cleanup(ResetProviders)
	RegisterProviderOp("acmeprov", "record_thing",
		func(context.Context, db.Stores, ConversationInfo, json.RawMessage) (any, error) {
			return nil, errors.New("handler blew up")
		})
	s := NewRelayServer(db.Stores{}, ConversationInfo{}, nil)

	// The send stage: the sidecar reporting it never delivered that provider's
	// record.
	CountUndeliveredRecords(context.Background(), "conv", []sidecarproto.UndeliveredRecords{
		{Namespace: "acmeprov", Op: "record_thing", Count: 1},
	})
	// The dispatch stage: the same op arriving and its handler failing.
	if err := relayRecord(t, s, "acmeprov", "record_thing", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("a failed provider record was answered %v; want it acknowledged", err)
	}

	got := collectDrops(t, reader)
	if n := got[dropStageRelaySend]["acmeprov.record_thing"]; n != 1 {
		t.Errorf("drops = %v; want one %s drop under op=acmeprov.record_thing", got, dropStageRelaySend)
	}
	if n := got[dropStageRelayDispatch]["acmeprov.record_thing"]; n != 1 {
		t.Errorf("drops = %v; want one %s drop under op=acmeprov.record_thing", got, dropStageRelayDispatch)
	}
}

// TestAuditDrop_RelayedRecordThatLandsCountsNothing: a record that decodes and
// writes is silent at every stage — the relay stages must not fire on the
// healthy path any more than the write stage does.
func TestAuditDrop_RelayedRecordThatLandsCountsNothing(t *testing.T) {
	reader := installTestAuditDrops(t)
	stores, info := newCaptureStores(t, true)
	s := NewRelayServer(stores, info, nil)

	args, _ := json.Marshal(agentproc.RecordGHWriteArgs{
		Method: "PUT", Path: "/repos/acme/widgets/pulls/7/merge", Status: 404,
	})
	if err := relayRecord(t, s, agentproc.RelayNamespaceCore, agentproc.OpRecordGHWrite, args); err != nil {
		t.Fatalf("relayRecord: %v", err)
	}

	if acts := listExternalActions(t, stores); len(acts) != 1 {
		t.Fatalf("want the relayed row written, got %d", len(acts))
	}
	if got := collectDrops(t, reader); len(got) != 0 {
		t.Fatalf("drops = %v; want none on a relay that landed", got)
	}
}

// TestAuditDrop_PanickingRecordIsCountedAndAcknowledged: a record whose
// handler panics would panic again on every resend, so it is counted at the
// dispatch stage and acknowledged rather than left to hold the records behind
// it.
func TestAuditDrop_PanickingRecordIsCountedAndAcknowledged(t *testing.T) {
	reader := installTestAuditDrops(t)
	t.Cleanup(ResetProviders)
	RegisterProviderOp("acmeprov", "record_thing",
		func(context.Context, db.Stores, ConversationInfo, json.RawMessage) (any, error) {
			panic("handler bug")
		})
	s := NewRelayServer(db.Stores{}, ConversationInfo{}, nil)

	if err := relayRecord(t, s, "acmeprov", "record_thing", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("a panicking record was answered %v; want it acknowledged", err)
	}
	if n := collectDrops(t, reader)[dropStageRelayDispatch]["acmeprov.record_thing"]; n != 1 {
		t.Fatalf("want one %s drop under op=acmeprov.record_thing", dropStageRelayDispatch)
	}
}

// TestRecordDropOp_LabelsOnlyTheVocabulary: a relayed record's deliverable
// kind and action are the sidecar's, so the write-stage drop label is either
// value only when it is in its domain vocabulary, and other when it is not.
func TestRecordDropOp_LabelsOnlyTheVocabulary(t *testing.T) {
	for _, tc := range []struct {
		name string
		a    *domain.Deliverable
		act  *domain.ExternalAction
		want string
	}{
		{"known kind", &domain.Deliverable{Kind: domain.DeliverableKindPullRequest}, nil, domain.DeliverableKindPullRequest},
		{"unknown kind", &domain.Deliverable{Kind: "forged"}, nil, dropOpOther},
		{"known action", nil, &domain.ExternalAction{Action: domain.ActionPRCreated}, domain.ActionPRCreated},
		{"unknown action", nil, &domain.ExternalAction{Action: "forged"}, dropOpOther},
	} {
		if got := recordDropOp(tc.a, tc.act); got != tc.want {
			t.Errorf("%s: recordDropOp = %q, want %q", tc.name, got, tc.want)
		}
	}
}
