package telemetry

import (
	"time"

	"go.opentelemetry.io/otel/attribute"
)

// The shared span-attribute vocabulary: every key TF sets itself (as
// opposed to otelhttp's http.* and otelsql's db.*), each an opaque
// identifier or a closed enum.
//
// Centralizing them is what makes that restriction enforceable. Spans
// leave the process for a backend the operator may not run, so an
// attribute is a data-egress decision, not a cardinality preference — repo
// names, titles, usernames, and file paths are tenant data and belong
// nowhere near one. A UUID plus TF's own database answers "which repo was
// that" for someone already entitled to the answer, and nobody else.
//
// attrs_test.go pins the exported set, so a new helper has to be argued
// for rather than appear.
const (
	keyOrgID          = attribute.Key("org.id")
	keyTeamID         = attribute.Key("team.id")
	keyEventID        = attribute.Key("event.id")
	keyEventType      = attribute.Key("event.type")
	keyEntityID       = attribute.Key("entity.id")
	keyTaskID         = attribute.Key("task.id")
	keyConversationID = attribute.Key("conversation.id")
	keyBlueprintRunID = attribute.Key("blueprint_run.id")
	keyClaimAttempt   = attribute.Key("claim.attempt")
	keySource         = attribute.Key("source")
	keyDisposition    = attribute.Key("disposition")
	keyOutcome        = attribute.Key("outcome")
	keyRuntime        = attribute.Key("runtime")
	keyAttempt        = attribute.Key("attempt")
	keyCount          = attribute.Key("count")
	keyJob            = attribute.Key("job")
	keyQueueWaitMS    = attribute.Key("queue.wait_ms")
	keyProvider       = attribute.Key("provider")
	keyTransport      = attribute.Key("transport")
	keyOp             = attribute.Key("op")
	keyWorkspace      = attribute.Key("workspace.provenance")
	keySizeBytes      = attribute.Key("size_bytes")
	keySnapBundle     = attribute.Key("snapshot.bundle_bytes")
	keySnapPatch      = attribute.Key("snapshot.patch_bytes")
	keySnapTranscript = attribute.Key("snapshot.transcript_bytes")
	keySnapRaw        = attribute.Key("snapshot.raw_bytes")
	keySnapWaitedMS   = attribute.Key("snapshot.waited_ms")
	keyAgentCostUSD   = attribute.Key("agent.cost_usd")
	keyAgentDuration  = attribute.Key("agent.duration_ms")
)

// Opaque row identifiers, the backbone of the set. Each is the id and
// nothing else — never the repo/number pair, the title, or the login that
// would name the thing to a reader. org.id in particular is what turns
// "the poll cycle was slow" into "slow for one tenant", since nearly every
// pipeline in TF fans out per-org.
func OrgID(id string) attribute.KeyValue          { return keyOrgID.String(id) }
func TeamID(id string) attribute.KeyValue         { return keyTeamID.String(id) }
func EventID(id string) attribute.KeyValue        { return keyEventID.String(id) }
func EntityID(id string) attribute.KeyValue       { return keyEntityID.String(id) }
func TaskID(id string) attribute.KeyValue         { return keyTaskID.String(id) }
func ConversationID(id string) attribute.KeyValue { return keyConversationID.String(id) }

// BlueprintRunID is the blueprint_runs row a span belongs to — a different
// table from ConversationID, and the reason both exist: the firing path
// spans a blueprint run whose steps are conversations minted later, so one
// key cannot honestly carry both. Opaque like the rest; a blueprint's name
// is not what it holds.
func BlueprintRunID(id string) attribute.KeyValue { return keyBlueprintRunID.String(id) }

// EventType is FK-constrained to the events catalog, which is what makes
// it safe as a dimension in a way no other event field is.
func EventType(kind string) attribute.KeyValue { return keyEventType.String(kind) }

// Closed enums, each fixed by its call sites:
//
//   - Source — the upstream a span talks to ("github", "jira"). The
//     poller/tracker's own dimension, not a hostname.
//   - Disposition — what a stage decided to do with its input (routed,
//     deduped, filtered).
//   - Runtime — which agent loop drove a run ("sdk", "native").
//   - Job — which background system job a span belongs to ("scorer",
//     "repo_profiler").
//   - Provider — an LLM vendor ("anthropic", "bedrock"), derived from
//     which credential shape resolved; never a hostname or a model id.
//   - Transport — how a call reached its upstream where a subsystem has
//     more than one route (the system-LLM path's "subprocess" vs "direct").
//   - Op — which operation a multiplexed channel carried, where one span
//     name covers a switch: a capbroker IPC method, a relay
//     "<namespace>.<op>" pair. Every value is a Go constant in the
//     dispatching switch, which is what keeps it an enum rather than a
//     name the caller chose.
//   - Workspace — how a run's worktree came to exist ("fresh", "warm",
//     "rehydrated"), i.e. domain.WorkspaceProvenance. Never the path.
func Source(name string) attribute.KeyValue       { return keySource.String(name) }
func Disposition(value string) attribute.KeyValue { return keyDisposition.String(value) }
func Runtime(value string) attribute.KeyValue     { return keyRuntime.String(value) }
func Job(name string) attribute.KeyValue          { return keyJob.String(name) }
func Provider(name string) attribute.KeyValue     { return keyProvider.String(name) }
func Transport(name string) attribute.KeyValue    { return keyTransport.String(name) }
func Op(name string) attribute.KeyValue           { return keyOp.String(name) }
func Workspace(value string) attribute.KeyValue   { return keyWorkspace.String(value) }

// Outcome names how a span finished. It exists to separate "failed" from
// "correctly declined to do anything" — provider backoff, a quiet-skip, a
// dedup hit — so the error status stays reserved for things actually
// wrong, and "traces with errors" stays a usable filter.
func Outcome(value string) attribute.KeyValue { return keyOutcome.String(value) }

// Counters. Attempt separates a span that spent four minutes in backoff
// from one slow request; ClaimAttempt says which engagement attempt
// produced a span; Count is a span's cardinality of work (PRs refreshed,
// events emitted) — a bare number, never a list of what was counted.
func Attempt(n int) attribute.KeyValue      { return keyAttempt.Int(n) }
func ClaimAttempt(n int) attribute.KeyValue { return keyClaimAttempt.Int(n) }
func Count(n int) attribute.KeyValue        { return keyCount.Int(n) }

// QueueWait is how long a durable-queue row sat between being enqueued and
// being claimed. It exists because that interval is over before the
// consumer's span begins, so nothing else in the trace can show it — and
// without it a backed-up queue and slow processing look the same. A
// duration, never a timestamp: when the row was written is not something a
// span needs to carry.
func QueueWait(d time.Duration) attribute.KeyValue {
	return keyQueueWaitMS.Int64(d.Milliseconds())
}

// SizeBytes is how much a span moved — a workspace snapshot's compressed
// footprint, say. A magnitude, never a filename or a key.
func SizeBytes(n int64) attribute.KeyValue { return keySizeBytes.Int64(n) }

// The workspace-snapshot member sizes, one per thing the snapshot carries,
// plus the pre-compression total. They exist because SizeBytes alone could
// not say where a slow snapshot's bytes came from: the git delta, the
// scratch tree, or the transcript are captured, archived, and uploaded by
// three different phases, and sizing a fix (shallower delta, faster codec,
// multipart PUT) needs the split. snapshot.raw_bytes against the archive
// span's SizeBytes is the compression ratio. All magnitudes — the member
// NAMES are fixed format strings, so no key here can carry tenant data.
func SnapshotBundleBytes(n int64) attribute.KeyValue     { return keySnapBundle.Int64(n) }
func SnapshotPatchBytes(n int64) attribute.KeyValue      { return keySnapPatch.Int64(n) }
func SnapshotTranscriptBytes(n int64) attribute.KeyValue { return keySnapTranscript.Int64(n) }
func SnapshotRawBytes(n int64) attribute.KeyValue        { return keySnapRaw.Int64(n) }

// SnapshotWaitedMs is how long a workspace ensure spent waiting on a persist
// that was still in flight, on the ensure span beside the provenance. Zero on
// every ensure that never had to wait, which is nearly all of them — the value
// is there so a resume that sat out most of a minute behind a hung writer
// reads differently from one that walked straight through, since the
// provenance it ends on is the same either way.
func SnapshotWaitedMs(ms int64) attribute.KeyValue { return keySnapWaitedMS.Int64(ms) }

// AgentCostUSD and AgentDuration are the agent runtime's OWN accounting of
// a finished engagement, carried onto the terminal span because neither is
// derivable from it: the span is punctual (it records a conclusion that
// happened during an unbounded streaming period the trace deliberately does
// not hold open), so its wall time is the bookkeeping, not the work. Both
// are already computed and persisted on the conversation row; putting them
// on the span is what lets "which runs were expensive" be asked of the same
// place as "which runs were slow to start".
func AgentCostUSD(usd float64) attribute.KeyValue { return keyAgentCostUSD.Float64(usd) }
func AgentDuration(ms int64) attribute.KeyValue   { return keyAgentDuration.Int64(ms) }
