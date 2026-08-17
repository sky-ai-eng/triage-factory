package delegate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// One provider for the whole test binary, and a cursor per test — the same
// reason internal/routing's tracing tests do it: a package-level tracer is
// created at init and the OTel global wires it to the FIRST provider
// installed in the process, never re-wiring. Per-test install/restore would
// quietly send later tests' spans to the first test's recorder.
var (
	traceOnce     sync.Once
	traceRecorder = tracetest.NewSpanRecorder()
)

// recordSpans arms tracing (once) and returns a reader for the spans that
// end after this call.
func recordSpans(t *testing.T) func() []sdktrace.ReadOnlySpan {
	t.Helper()
	traceOnce.Do(func() {
		otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(traceRecorder)))
	})
	start := len(traceRecorder.Ended())
	return func() []sdktrace.ReadOnlySpan { return traceRecorder.Ended()[start:] }
}

func spansNamed(spans []sdktrace.ReadOnlySpan, name string) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range spans {
		if s.Name() == name {
			out = append(out, s)
		}
	}
	return out
}

func spanAttr(t *testing.T, s sdktrace.ReadOnlySpan, key string) attribute.Value {
	t.Helper()
	for _, kv := range s.Attributes() {
		if string(kv.Key) == key {
			return kv.Value
		}
	}
	t.Fatalf("span %q has no %q attribute (has: %v)", s.Name(), key, s.Attributes())
	return attribute.Value{}
}

func traceTestConversation(id string) *domain.Conversation {
	return &domain.Conversation{
		ID:       id,
		OrgID:    runmode.LocalDefaultOrgID,
		TaskID:   "task-" + id,
		Runtime:  domain.ConversationRuntimeSDK,
		Attempts: 2,
	}
}

// TestEngagementRootEndsAtAgentLive is the ticket's central claim: the root
// span covers claim → agent-live and ENDS there, rather than being held open
// across the streaming period. A span that has not ended has not exported,
// so "ends at agent-live" is what makes a crash mid-stream still yield the
// setup trace.
func TestEngagementRootEndsAtAgentLive(t *testing.T) {
	read := recordSpans(t)
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	conv := traceTestConversation("run-live")

	_, done := s.beginEngagement(context.Background(), conv)
	if got := spansNamed(read(), "engagement.setup"); len(got) != 0 {
		t.Fatalf("the root exported before agent-live (%d spans); it must not end until the phase clears", len(got))
	}

	// The phase-clear to running IS agent-live, and it is the one signal both
	// runtimes share. s.conversations is nil here, so updatePhase's write no-ops
	// and this covers exactly the span half.
	s.endEngagement(conv.ID, engagementLive)
	done()

	roots := spansNamed(read(), "engagement.setup")
	if len(roots) != 1 {
		t.Fatalf("engagement.setup spans = %d, want exactly 1 per claim attempt", len(roots))
	}
	root := roots[0]
	if got := spanAttr(t, root, "outcome").AsString(); got != engagementLive {
		t.Errorf("outcome = %q, want %q — the deferred cleanup must not overwrite the agent-live end", got, engagementLive)
	}
	if got := spanAttr(t, root, "conversation.id").AsString(); got != conv.ID {
		t.Errorf("conversation.id = %q, want %q", got, conv.ID)
	}
	if got := spanAttr(t, root, "task.id").AsString(); got != conv.TaskID {
		t.Errorf("task.id = %q, want %q", got, conv.TaskID)
	}
	if got := spanAttr(t, root, "org.id").AsString(); got != conv.OrgID {
		t.Errorf("org.id = %q, want %q", got, conv.OrgID)
	}
	if got := spanAttr(t, root, "claim.attempt").AsInt64(); got != 2 {
		t.Errorf("claim.attempt = %d, want 2", got)
	}
	// runtime is on the root because everything it covers is upstream of the
	// runtime branch: a native run's setup is traced even though its loop is
	// deliberately out of scope.
	if got := spanAttr(t, root, "runtime").AsString(); got != domain.ConversationRuntimeSDK {
		t.Errorf("runtime = %q, want %q", got, domain.ConversationRuntimeSDK)
	}
	if root.Parent().IsValid() {
		t.Error("the engagement root has a parent; it must be a fresh root so the dispatcher's drain loop doesn't accrete one unbounded trace")
	}
}

// TestEngagementRootRecordsSetupFailure: a claim that never reaches the agent
// ends the root with an error status, and that is the ONLY outcome that does
// — every other early end is a disposition, so "traces with errors" stays a
// usable filter.
func TestEngagementRootRecordsSetupFailure(t *testing.T) {
	read := recordSpans(t)
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	conv := traceTestConversation("run-fail")

	_, done := s.beginEngagement(context.Background(), conv)
	s.failEngagement(conv.ID, errors.New("bring up credential sidecar: broker refused"))
	done()

	roots := spansNamed(read(), "engagement.setup")
	if len(roots) != 1 {
		t.Fatalf("engagement.setup spans = %d, want 1", len(roots))
	}
	if got := spanAttr(t, roots[0], "outcome").AsString(); got != engagementSetupFailed {
		t.Errorf("outcome = %q, want %q", got, engagementSetupFailed)
	}
	if got := roots[0].Status().Code; got != codes.Error {
		t.Errorf("status = %v, want Error — a setup failure is the one engagement end that is actually wrong", got)
	}
}

// TestEngagementDispositionsAreNotErrors covers the other half of that rule:
// a raced cancel and a message-less follow-up park are anticipated, so they
// name themselves in the outcome and leave the status unset.
func TestEngagementDispositionsAreNotErrors(t *testing.T) {
	for _, tc := range []struct{ name, outcome string }{
		{"cancelled", engagementCancelled},
		{"no-message", engagementNoMessage},
		{"fenced", engagementFenced},
		{"never-started", engagementNotStarted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			read := recordSpans(t)
			s := NewSpawner(nil, db.Stores{}, nil, nil, "")
			conv := traceTestConversation("run-" + tc.name)

			_, done := s.beginEngagement(context.Background(), conv)
			if tc.outcome != engagementNotStarted {
				s.endEngagement(conv.ID, tc.outcome)
			}
			done()

			roots := spansNamed(read(), "engagement.setup")
			if len(roots) != 1 {
				t.Fatalf("engagement.setup spans = %d, want 1", len(roots))
			}
			if got := spanAttr(t, roots[0], "outcome").AsString(); got != tc.outcome {
				t.Errorf("outcome = %q, want %q", got, tc.outcome)
			}
			if got := roots[0].Status().Code; got == codes.Error {
				t.Errorf("status = Error for an anticipated disposition; only a real setup failure may set it")
			}
		})
	}
}

// TestStoppedEngagementNamesWhichCancellation covers the exits that read a
// cancelled context and park the run. The code treats a user stop and a
// dispatcher shutdown alike — both mean "stop, leave the workspace alone" —
// but they are different events, and a trace that reported both as the
// unexplained not_started could not answer the question these spans are
// usually opened for: was this run stopped, or did its executor go away?
//
// The discriminator is which context is done. step derives from parent, so a
// shutdown cancels both and a user stop cancels only step — which is why
// stopOutcome must read the parent FIRST.
func TestStoppedEngagementNamesWhichCancellation(t *testing.T) {
	userStop := func() (context.Context, context.Context) {
		parent := context.Background()
		step, cancel := context.WithCancel(parent)
		cancel() // the run's own cancel handle — Spawner.Stop
		return parent, step
	}
	shutdown := func() (context.Context, context.Context) {
		parent, cancel := context.WithCancel(context.Background())
		step, stepCancel := context.WithCancel(parent)
		t.Cleanup(stepCancel)
		cancel() // the dispatcher's ctx — which cancels step too
		return parent, step
	}
	live := func() (context.Context, context.Context) {
		parent := context.Background()
		step, cancel := context.WithCancel(parent)
		t.Cleanup(cancel)
		return parent, step
	}

	for _, tc := range []struct {
		name    string
		ctxs    func() (context.Context, context.Context)
		want    string
		wantEnd bool
	}{
		{"user stop", userStop, engagementCancelled, true},
		{"dispatcher shutdown", shutdown, engagementShutdown, true},
		{"nothing cancelled", live, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			read := recordSpans(t)
			s := NewSpawner(nil, db.Stores{}, nil, nil, "")
			conv := traceTestConversation("run-stop-" + tc.name)
			parent, step := tc.ctxs()

			_, done := s.beginEngagement(context.Background(), conv)
			s.endEngagementIfStopped(conv.ID, parent, step)

			if !tc.wantEnd {
				// Nothing was cancelled, so this is not a stop and the root must
				// still be open — claiming the end here would silently no-op the
				// launch-error or agent-live end that actually owns it.
				if got := spansNamed(read(), "engagement.setup"); len(got) != 0 {
					t.Fatalf("the root ended on a non-stop path (%d spans); the real end would then be swallowed", len(got))
				}
				done()
				return
			}

			done() // a no-op if the stop already ended it, which is the point
			roots := spansNamed(read(), "engagement.setup")
			if len(roots) != 1 {
				t.Fatalf("engagement.setup spans = %d, want 1", len(roots))
			}
			if got := spanAttr(t, roots[0], "outcome").AsString(); got != tc.want {
				t.Errorf("outcome = %q, want %q — the deferred cleanup's not_started must not win over a named stop", got, tc.want)
			}
			if roots[0].Status().Code == codes.Error {
				t.Error("a stop was recorded as an error; it is a disposition someone asked for")
			}
		})
	}
}

// TestRequeuedRunProducesSeparateTracesSharingConversationID is the ticket's
// acceptance criterion for retries: five attempts of one conversation are
// five navigable traces, joined by conversation.id and told apart by
// claim.attempt — not one sprawling trace.
func TestRequeuedRunProducesSeparateTracesSharingConversationID(t *testing.T) {
	read := recordSpans(t)
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")

	for attempt := 1; attempt <= maxClaimAttempts; attempt++ {
		conv := traceTestConversation("run-requeued")
		conv.Attempts = attempt
		_, done := s.beginEngagement(context.Background(), conv)
		s.failEngagement(conv.ID, errors.New("transient"))
		done()
	}

	roots := spansNamed(read(), "engagement.setup")
	if len(roots) != maxClaimAttempts {
		t.Fatalf("engagement.setup spans = %d, want %d — one per attempt", len(roots), maxClaimAttempts)
	}
	traces := map[trace.TraceID]bool{}
	attempts := map[int64]bool{}
	for _, r := range roots {
		traces[r.SpanContext().TraceID()] = true
		attempts[spanAttr(t, r, "claim.attempt").AsInt64()] = true
		if got := spanAttr(t, r, "conversation.id").AsString(); got != "run-requeued" {
			t.Errorf("conversation.id = %q, want the shared id", got)
		}
	}
	if len(traces) != maxClaimAttempts {
		t.Errorf("distinct trace ids = %d, want %d — attempts must not share a trace", len(traces), maxClaimAttempts)
	}
	if len(attempts) != maxClaimAttempts {
		t.Errorf("distinct claim.attempt values = %d, want %d", len(attempts), maxClaimAttempts)
	}
}

// TestPunctualSpansLinkToTheEngagementRoot: everything after agent-live is a
// fresh root carrying a link, not a child. Parenting would either reopen the
// ended root or grow one trace across an unbounded streaming period; a link
// records the origin and leaves each side independently exportable.
func TestPunctualSpansLinkToTheEngagementRoot(t *testing.T) {
	read := recordSpans(t)
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	conv := traceTestConversation("run-punctual")

	_, done := s.beginEngagement(context.Background(), conv)
	rootSC := s.engagementSpanContext(conv.ID)
	if !rootSC.IsValid() {
		t.Fatal("the engagement's SpanContext was not retained; nothing after agent-live could link back")
	}
	s.endEngagement(conv.ID, engagementLive)

	// After the root has ENDED, which is the real condition these spans run
	// under.
	_, span := s.startPunctual(context.Background(), conv.ID, "permission.prompt")
	span.End()
	done()

	punctual := spansNamed(read(), "permission.prompt")
	if len(punctual) != 1 {
		t.Fatalf("permission.prompt spans = %d, want 1", len(punctual))
	}
	p := punctual[0]
	if p.Parent().IsValid() {
		t.Error("the punctual span has a parent; it must be a fresh root (standing decision 5)")
	}
	if len(p.Links()) != 1 {
		t.Fatalf("links = %d, want 1 back to the engagement root", len(p.Links()))
	}
	if got := p.Links()[0].SpanContext.SpanID(); got != rootSC.SpanID() {
		t.Errorf("link points at span %v, want the engagement root %v", got, rootSC.SpanID())
	}
	// Links do not share the parent's sampling decision, so a punctual span
	// has to be self-sufficient about which run it belongs to.
	if got := spanAttr(t, p, "conversation.id").AsString(); got != conv.ID {
		t.Errorf("conversation.id = %q, want %q — a link may not survive, the attribute must", got, conv.ID)
	}
}

// TestPunctualSpanWithoutAnEngagementIsUnlinked: tracing is optional and the
// absence of an engagement is never an error — a local-mode park, a curator
// turn, a test fixture. The span still exists and still names its run.
func TestPunctualSpanWithoutAnEngagementIsUnlinked(t *testing.T) {
	read := recordSpans(t)
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")

	_, span := s.startPunctual(context.Background(), "run-orphan", "workspace.snapshot")
	span.End()

	got := spansNamed(read(), "workspace.snapshot")
	if len(got) != 1 {
		t.Fatalf("workspace.snapshot spans = %d, want 1", len(got))
	}
	if len(got[0].Links()) != 0 {
		t.Errorf("links = %d, want 0 for a run with no live engagement", len(got[0].Links()))
	}
	if v := spanAttr(t, got[0], "conversation.id").AsString(); v != "run-orphan" {
		t.Errorf("conversation.id = %q, want run-orphan", v)
	}
}

// TestEngagementTeardownIsIdentityChecked: a re-claim of the same
// conversation registers its own engagement, and the predecessor's deferred
// teardown must not deregister it — otherwise the successor's punctual spans
// would silently stop linking.
func TestEngagementTeardownIsIdentityChecked(t *testing.T) {
	recordSpans(t)
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	conv := traceTestConversation("run-reclaim")

	_, firstDone := s.beginEngagement(context.Background(), conv)
	first := s.engagementSpanContext(conv.ID)

	_, secondDone := s.beginEngagement(context.Background(), conv)
	second := s.engagementSpanContext(conv.ID)
	if first.SpanID() == second.SpanID() {
		t.Fatal("the successor did not replace the registered engagement")
	}

	firstDone() // the predecessor's teardown, arriving late
	if got := s.engagementSpanContext(conv.ID); got.SpanID() != second.SpanID() {
		t.Errorf("after the predecessor's teardown the registered engagement is %v, want the successor's %v", got.SpanID(), second.SpanID())
	}
	secondDone()
	if s.engagementSpanContext(conv.ID).IsValid() {
		t.Error("the successor's teardown left its engagement registered; the map would grow per run id")
	}
}

// TestNoWireProtocolCarriesTraceContext is the ticket's fourth acceptance
// criterion, as a test rather than a review note. Standing decision 1 is that
// the privileged processes never see trace context: the cap-broker holds
// CAP_SYS_ADMIN and must gain no outbound telemetry dependency, and the
// sidecar's egress is fail-closed. Both are enforced by the frames simply
// never having a field for it — which is exactly the kind of absence a
// future change removes without noticing.
func TestNoWireProtocolCarriesTraceContext(t *testing.T) {
	// The three cross-process wire formats a delegated run uses: the
	// cap-broker envelope, the sidecar supervision frames, and the agent's
	// exec-verb socket protocol.
	protocols := []string{
		"cmd/capbroker/protocol.go",
		"internal/sidecarproto",
		"cmd/exec/agenthost",
	}
	// Substrings that would mean a frame is carrying W3C trace context.
	banned := []string{"traceparent", "tracestate", "TraceParent", "TraceState", "SpanContext"}

	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	for _, target := range protocols {
		for _, file := range goFilesUnder(t, filepath.Join(root, target)) {
			// tracing.go is where a package's own client-side spans live;
			// it writes no frames.
			if strings.HasSuffix(file, "tracing.go") || strings.HasSuffix(file, "_test.go") {
				continue
			}
			body, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("read %s: %v", file, err)
			}
			for _, word := range banned {
				// The RelayServer holds the engagement's SpanContext as a
				// FIELD, captured in-process at construction — the whole
				// point being that it never crosses a socket. That one
				// mention is legitimate; the traceparent/tracestate words
				// are still banned there, because those only ever appear
				// when something is being serialized.
				if word == "SpanContext" && strings.HasSuffix(file, "relayserver.go") {
					continue
				}
				if strings.Contains(string(body), word) {
					t.Errorf("%s mentions %q: no wire protocol crossing into the cap-broker or the sidecar may carry trace context (standing decision 1) — the privileged processes stay span-free, and their work is timed by client spans in the executor",
						strings.TrimPrefix(file, root+"/"), word)
				}
			}
		}
	}
}

func goFilesUnder(t *testing.T, path string) []string {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if !info.IsDir() {
		return []string{path}
	}
	var out []string
	err = filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(p, ".go") {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", path, err)
	}
	if len(out) == 0 {
		t.Fatalf("no Go files under %s — the guard would pass vacuously", path)
	}
	return out
}
