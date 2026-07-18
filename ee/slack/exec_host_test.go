package slack

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// fakeRelayRuntime is a minimal agenthost.ExtensionRuntime fake that lets a
// test script Relay's per-call outcome — used to pin recordThreadRoot's
// retry behavior without a real store/relay round trip. relayErrs is
// consumed in call order; once exhausted, Relay succeeds.
type fakeRelayRuntime struct {
	relayErrs  []error
	relayCalls int
}

func (f *fakeRelayRuntime) Info() agenthost.RunInfo { return agenthost.RunInfo{} }

func (f *fakeRelayRuntime) Relay(_ context.Context, _, _ string, _, _ any) error {
	i := f.relayCalls
	f.relayCalls++
	if i < len(f.relayErrs) {
		return f.relayErrs[i]
	}
	return nil
}

func (f *fakeRelayRuntime) RelayNotify(context.Context, string, string, any) {}

func (f *fakeRelayRuntime) ProviderCredential(context.Context, string) (json.RawMessage, error) {
	return nil, nil
}

func (f *fakeRelayRuntime) Record(context.Context, *domain.Artifact, *domain.ExternalAction) {}

var _ agenthost.ExtensionRuntime = (*fakeRelayRuntime)(nil)

// TestRecordThreadRoot_RetriesTransientFailures pins that recordThreadRoot
// retries a failed relay call rather than giving up on the first error —
// the mitigation for a failure here otherwise permanently mis-tagging the
// thread's entity kind="message" instead of "thread" (see the touched-entity
// resolver in cmd/exec/agenthost/record.go).
func TestRecordThreadRoot_RetriesTransientFailures(t *testing.T) {
	rt := &fakeRelayRuntime{relayErrs: []error{errors.New("transient"), errors.New("transient")}}
	h := &slackExecHandler{}
	ws := slackWorkspaceIdentity{WorkspaceID: "T1", APIAppID: "A1"}

	err := h.recordThreadRoot(context.Background(), rt, ws, "C1", "1700000000.000100", "hello")
	if err != nil {
		t.Fatalf("recordThreadRoot: %v", err)
	}
	if rt.relayCalls != 3 {
		t.Errorf("relay calls = %d, want 3 (2 failures then a success)", rt.relayCalls)
	}
}

// TestRecordThreadRoot_ExhaustsRetriesAndReturnsError pins the other half:
// once every attempt fails, recordThreadRoot surfaces the error (rather than
// swallowing it) so send() can report it — a silent failure here has a
// permanent consequence, unlike most best-effort writes in this package.
func TestRecordThreadRoot_ExhaustsRetriesAndReturnsError(t *testing.T) {
	rt := &fakeRelayRuntime{relayErrs: []error{errors.New("down"), errors.New("down"), errors.New("down")}}
	h := &slackExecHandler{}
	ws := slackWorkspaceIdentity{WorkspaceID: "T1", APIAppID: "A1"}

	err := h.recordThreadRoot(context.Background(), rt, ws, "C1", "1700000000.000100", "hello")
	if err == nil {
		t.Fatal("expected an error once every retry is exhausted")
	}
	if rt.relayCalls != recordThreadRootMaxAttempts {
		t.Errorf("relay calls = %d, want %d", rt.relayCalls, recordThreadRootMaxAttempts)
	}
}

// TestParseSlackTSParts pins the (seconds, fractional-nanoseconds) decomposition
// parseSlackTSParts uses instead of a float64 parse — see sortMessagesByTS's
// doc for why a parsed float64 risks misordering two close-together Slack
// timestamps.
func TestParseSlackTSParts(t *testing.T) {
	cases := []struct {
		ts           string
		wantSeconds  int64
		wantFracNano int64
	}{
		{"1355517523.000005", 1355517523, 5000},
		{"1700000000.000001", 1700000000, 1000},
		{"1700000000.000002", 1700000000, 2000},
		{"1700000000", 1700000000, 0},
		{"", 0, 0},
		{"not-a-number", 0, 0},
	}
	for _, c := range cases {
		sec, frac := parseSlackTSParts(c.ts)
		if sec != c.wantSeconds || frac != c.wantFracNano {
			t.Errorf("parseSlackTSParts(%q) = (%d, %d), want (%d, %d)", c.ts, sec, frac, c.wantSeconds, c.wantFracNano)
		}
	}
}

// TestSortMessagesByTS_SameSecondDistinctMicroseconds pins that messages
// sent within the same second sort correctly by their fractional part — the
// exact scenario a parsed-float64 comparison risks collapsing/misordering,
// since a 10-digit-seconds + 6-digit-fraction Slack ts sits at float64's
// precision ceiling (~15-17 significant decimal digits).
func TestSortMessagesByTS_SameSecondDistinctMicroseconds(t *testing.T) {
	msgs := []slackMessage{
		{TS: "1700000000.000009", Text: "third"},
		{TS: "1700000000.000001", Text: "first"},
		{TS: "1700000000.000005", Text: "second"},
	}
	sortMessagesByTS(msgs)
	want := []string{"first", "second", "third"}
	for i, m := range msgs {
		if m.Text != want[i] {
			t.Errorf("position %d = %q, want %q (full order: %+v)", i, m.Text, want[i], msgs)
		}
	}
}

// TestSortMessagesByTS_DistinctSeconds pins ordinary cross-second ordering
// still works (the common case, unaffected by the fractional-precision fix).
func TestSortMessagesByTS_DistinctSeconds(t *testing.T) {
	msgs := []slackMessage{
		{TS: "1700000002.000000", Text: "third"},
		{TS: "1700000000.500000", Text: "first"},
		{TS: "1700000001.000000", Text: "second"},
	}
	sortMessagesByTS(msgs)
	want := []string{"first", "second", "third"}
	for i, m := range msgs {
		if m.Text != want[i] {
			t.Errorf("position %d = %q, want %q (full order: %+v)", i, m.Text, want[i], msgs)
		}
	}
}
