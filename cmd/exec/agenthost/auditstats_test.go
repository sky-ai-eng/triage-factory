package agenthost

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
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
		Provider:   domain.ArtifactProviderGitHub,
		Action:     domain.ActionGHChannelWrite,
		Target:     "acme/widgets",
		Credential: domain.CredentialGitHubApp,
	})

	got := collectDrops(t, reader)
	if n := got[dropStageRecord][domain.ActionGHChannelWrite]; n != 1 {
		t.Fatalf("drops = %v; want one %s drop under op=%s", got, dropStageRecord, domain.ActionGHChannelWrite)
	}
}

// TestAuditDrop_RecordFailureNamesTheArtifactKind: an artifact-bearing write
// is named by its kind rather than the action's, matching the attribute the
// artifact.record span at the same site already sets.
func TestAuditDrop_RecordFailureNamesTheArtifactKind(t *testing.T) {
	reader := installTestAuditDrops(t)
	conn, stores, info := newCaptureStoresConn(t, true)
	if err := conn.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	RecordExternalWrite(context.Background(), stores, info,
		&domain.Artifact{Provider: domain.ArtifactProviderGitHub, Kind: domain.ArtifactKindPullRequest, Target: "acme/widgets#7"},
		&domain.ExternalAction{Provider: domain.ArtifactProviderGitHub, Action: domain.ActionPRCreated, Target: "acme/widgets#7"})

	got := collectDrops(t, reader)
	if n := got[dropStageRecord][domain.ArtifactKindPullRequest]; n != 1 {
		t.Fatalf("drops = %v; want one %s drop under op=%s", got, dropStageRecord, domain.ArtifactKindPullRequest)
	}
}

// TestAuditDrop_SuccessfulWriteCountsNothing: the counter must stay at zero on
// the path every run takes, or "did we lose anything" has no answer.
func TestAuditDrop_SuccessfulWriteCountsNothing(t *testing.T) {
	reader := installTestAuditDrops(t)
	stores, info := newCaptureStores(t, true)

	RecordExternalWrite(context.Background(), stores, info, nil, &domain.ExternalAction{
		Provider:   domain.ArtifactProviderGitHub,
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
// acceptance: a notify whose payload never becomes a record is counted at the
// decode stage, naming the op it arrived under.
func TestAuditDrop_RelayDecodeFailureIsCounted(t *testing.T) {
	reader := installTestAuditDrops(t)
	stores, info := newCaptureStores(t, true)
	s := NewRelayServer(stores, info, nil)

	// Well-formed JSON, wrong shape for this op — the failure a version skew or
	// a corrupted frame produces, rather than a syntactically broken one.
	s.DispatchNotify(context.Background(), agentproc.RelayNamespaceCore, agentproc.OpRecordGHWrite,
		json.RawMessage(`{"status":"not-an-int"}`))

	if acts := listExternalActions(t, stores); len(acts) != 0 {
		t.Fatalf("an undecodable notify wrote %d rows; want none", len(acts))
	}
	got := collectDrops(t, reader)
	wantOp := agentproc.RelayNamespaceCore + "." + agentproc.OpRecordGHWrite
	if n := got[dropStageRelayDecode][wantOp]; n != 1 {
		t.Fatalf("drops = %v; want one %s drop under op=%s", got, dropStageRelayDecode, wantOp)
	}
}

// TestAuditDrop_UnsupportedNotifyOpIsCounted: an op with no case here lost a
// record just as surely as an undecodable one, and its name came off the wire,
// so it is counted under the clamp rather than verbatim.
func TestAuditDrop_UnsupportedNotifyOpIsCounted(t *testing.T) {
	reader := installTestAuditDrops(t)
	s := NewRelayServer(db.Stores{}, RunInfo{}, nil)

	s.DispatchNotify(context.Background(), agentproc.RelayNamespaceCore, "record_something_invented", json.RawMessage(`{}`))

	got := collectDrops(t, reader)
	if n := got[dropStageRelayDecode][dropOpOther]; n != 1 {
		t.Fatalf("drops = %v; want one %s drop under op=%s", got, dropStageRelayDecode, dropOpOther)
	}
}

// TestAuditDrop_SidecarSendReportIsCounted: the sidecar's report that a record
// never reached the wire lands at the SEND stage, not the receive one — the
// frame carrying the report arrived fine; the record it describes did not.
func TestAuditDrop_SidecarSendReportIsCounted(t *testing.T) {
	reader := installTestAuditDrops(t)
	s := NewRelayServer(db.Stores{}, RunInfo{}, nil)

	args, _ := json.Marshal(agentproc.RecordRelayDropArgs{
		Namespace: agentproc.RelayNamespaceCore,
		Op:        agentproc.OpRecordGHWrite,
	})
	s.DispatchNotify(context.Background(), agentproc.RelayNamespaceCore, agentproc.OpRecordRelayDrop, args)

	got := collectDrops(t, reader)
	wantOp := agentproc.RelayNamespaceCore + "." + agentproc.OpRecordGHWrite
	if n := got[dropStageRelaySend][wantOp]; n != 1 {
		t.Fatalf("drops = %v; want one %s drop under op=%s", got, dropStageRelaySend, wantOp)
	}
	if _, counted := got[dropStageRelayDecode]; counted {
		t.Errorf("drops = %v; the report's own frame decoded fine and must not count", got)
	}
}

// TestAuditDrop_WireOpNamesAreClamped: the op label arrives from the process
// deliberately exposed to hostile text, so anything outside the known set
// collapses to a single series. Unclamped, naming ops that do not exist would
// be an unbounded-cardinality write into the orchestrator's exporter.
func TestAuditDrop_WireOpNamesAreClamped(t *testing.T) {
	reader := installTestAuditDrops(t)
	s := NewRelayServer(db.Stores{}, RunInfo{}, nil)

	for _, a := range []agentproc.RecordRelayDropArgs{
		{Namespace: agentproc.RelayNamespaceCore, Op: "invented_one"},
		{Namespace: agentproc.RelayNamespaceCore, Op: "invented_two"},
		// An unregistered namespace: nothing serves it, so there is no name to
		// vouch for.
		{Namespace: "not_a_provider", Op: agentproc.OpRecordGHWrite},
	} {
		args, _ := json.Marshal(a)
		s.DispatchNotify(context.Background(), agentproc.RelayNamespaceCore, agentproc.OpRecordRelayDrop, args)
	}

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
		func(context.Context, db.Stores, RunInfo, json.RawMessage) (any, error) {
			return nil, errors.New("handler blew up")
		})
	s := NewRelayServer(db.Stores{}, RunInfo{}, nil)

	// The send stage: the sidecar reporting it could not relay that provider's
	// record.
	args, _ := json.Marshal(agentproc.RecordRelayDropArgs{Namespace: "acmeprov", Op: "record_thing"})
	s.DispatchNotify(context.Background(), agentproc.RelayNamespaceCore, agentproc.OpRecordRelayDrop, args)
	// The dispatch stage: the same op arriving and its handler failing.
	s.DispatchNotify(context.Background(), "acmeprov", "record_thing", json.RawMessage(`{}`))

	got := collectDrops(t, reader)
	if n := got[dropStageRelaySend]["acmeprov.record_thing"]; n != 1 {
		t.Errorf("drops = %v; want one %s drop under op=acmeprov.record_thing", got, dropStageRelaySend)
	}
	if n := got[dropStageRelayDispatch]["acmeprov.record_thing"]; n != 1 {
		t.Errorf("drops = %v; want one %s drop under op=acmeprov.record_thing", got, dropStageRelayDispatch)
	}
}

// TestAuditDrop_RelayedRecordThatLandsCountsNothing: a notify that decodes and
// writes is silent at every stage — the relay stages must not fire on the
// healthy path any more than the write stage does.
func TestAuditDrop_RelayedRecordThatLandsCountsNothing(t *testing.T) {
	reader := installTestAuditDrops(t)
	stores, info := newCaptureStores(t, true)
	s := NewRelayServer(stores, info, nil)

	args, _ := json.Marshal(agentproc.RecordGHWriteArgs{
		Method: "PUT", Path: "/repos/acme/widgets/pulls/7/merge", Status: 404,
	})
	s.DispatchNotify(context.Background(), agentproc.RelayNamespaceCore, agentproc.OpRecordGHWrite, args)

	if acts := listExternalActions(t, stores); len(acts) != 1 {
		t.Fatalf("want the relayed row written, got %d", len(acts))
	}
	if got := collectDrops(t, reader); len(got) != 0 {
		t.Fatalf("drops = %v; want none on a relay that landed", got)
	}
}
