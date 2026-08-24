// Package modelprobe answers, for one org and one model, whether the org's
// credentials can actually invoke it — by invoking it.
//
// The question has no uniform control-plane answer. Anthropic's /v1/models
// reflects the key's access, so there it would work. Bedrock's
// ListFoundationModels reports every model the REGION carries, not the ones
// the account was granted, and none of its filters touch entitlement; its
// per-model GetFoundationModelAvailability exists but is one call per model,
// needs its own IAM action on a different service endpoint, and answers a
// weaker question (an agreement record across four axes) than "can this
// credential call this id right now". It also lists base model ids while the
// current Claude models can only be invoked through inference profiles, so the
// ids you can enumerate and the ids you can call are different sets.
//
// A probe costs the same number of calls, needs no permission the run does not
// already need — if the probe fails, the run would have failed too — works
// identically for every vendor instead of needing an integration each, and
// tests the exact string that will go on the wire.
//
// What it costs is real money, which is why nothing here runs on a timer, at
// boot, or on a poll cycle: every probe is caused by a person, and the surface
// that causes one says so first.
package modelprobe

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/inference"
	"github.com/sky-ai-eng/triage-factory/internal/logging"
	"github.com/sky-ai-eng/triage-factory/internal/modelcatalog"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/systemllm"
)

var log = logging.Component("modelprobe")

// probeTimeout bounds one probe spent directly from this process. A one-token
// completion that has not answered in this long is not going to say anything
// about entitlement, and a sweep of a provider's candidates runs them one after
// another inside a single HTTP request — so the bound is what keeps a wedged
// upstream from holding the admin's browser open for minutes to reach the same
// inconclusive it would reach here.
const probeTimeout = 15 * time.Second

// probeSDKTimeout bounds one probe spent through the agent runtime. It is
// wider than the direct bound because it covers more than a completion: the
// node subprocess has to boot, and on a deployment whose first act is pressing
// test, agentproc installs the runtime before it can send anything. A warm
// invocation answers in a few seconds; this is sized for the cold one, so a
// fresh install does not report every model inconclusive.
//
// Still narrower than a retrying upstream, which is the point of having a
// bound at all: the runtime retries a 5xx for around three minutes before
// giving up, and both numbers reach the same inconclusive.
const probeSDKTimeout = 60 * time.Second

// probeMessage is the whole prompt. The question is whether the request is
// ACCEPTED, so the cheapest content that is still a valid turn is the right
// content; nothing reads the answer.
const probeMessage = "ping"

// probeMaxTokens caps the completion at one token. Anthropic requires
// max_tokens, and the request is billed on what it generates, so this is the
// floor of what the probe can cost.
const probeMaxTokens = 1

// probeMaxTurns bounds the SDK path at a single assistant turn. The probe
// allowlists no tools, so one turn is all a "ping" can take; the flag is what
// stops a runtime that decides to try one anyway from burning the timeout to
// reach the same answer.
const probeMaxTurns = 1

// runSDK is the SDK-subprocess seam, swapped out in tests so the local branch
// is verifiable without spawning node.
var runSDK = agentproc.Run

// Result is one probe's conclusion.
type Result struct {
	// Verdict is green, red, or inconclusive. Only the first two are stored.
	Verdict Verdict
	// Detail is the provider's own message on a non-green verdict, carried so
	// an admin reads WHY rather than just that. Empty on green.
	Detail string
}

// Prober runs availability probes for an org against its own credentials.
//
// It holds the same two credential seams every other brain-side LLM caller
// does — the raw secret reader and the role-aware resolver — because a
// role-mode Bedrock org stores no key at all and must mint an STS triple for
// this call exactly as a run would. Probing through a different path than runs
// use would test something other than what runs do.
//
// Both are nil in local mode, where runs resolve nothing here either: the
// subprocess authenticates from the host environment, and handing it the same
// two nils a run gets is what keeps the probe honest about that.
type Prober struct {
	secrets  agentproc.SecretsReader
	resolver func(ctx context.Context, orgID, model string) (map[string]string, error)
	recorder *systemllm.Recorder
}

// New wires a Prober. resolver may be nil, in which case credentials come from
// the secret store directly (the same fallback every agentproc caller has).
// recorder may be nil, which only means the probe's spend goes unrecorded —
// the probe itself still runs, because a ledger outage must not be able to
// block an admin from establishing what their credentials can do.
func New(secrets agentproc.SecretsReader, resolver func(ctx context.Context, orgID, model string) (map[string]string, error), recorder *systemllm.Recorder) *Prober {
	return &Prober{secrets: secrets, resolver: resolver, recorder: recorder}
}

// Probe spends one minimal request establishing whether orgID's credentials
// can invoke m.
//
// m comes from the deployment's own universe, so its key is already in the
// vocabulary the transport below speaks — a bifrost wire id on the direct path,
// a harness alias on the SDK one — and nothing here translates it.
//
// The returned error is for the cases where the question was never asked at
// all: the org has not connected this model's provider, or its stored material
// cannot be assembled into a request. Those are not verdicts — nothing is
// recorded for them and the caller reports a setup gap, not an unavailable
// model. Every outcome of an attempt that DID reach the wire comes back as a
// Result, including the failures.
//
// What actually varies is the RUNTIME, and the two paths here are the two
// conversation runtimes' own machinery: the direct one is inference.New over a
// resolved env map, which is what agentloop.EnvCredentials builds per call for
// the native engine, and the SDK one is agentproc.Run. So a probe spends its
// request through the same client a delegated run does, and tests the
// credential path that run will take rather than a parallel one.
//
// It selects on the mode because the mode is what settles the runtime: the
// dialect IS the mode, and the enqueue stamps the ratchet per dialect —
// Postgres mints delegations 'native', SQLite mints them 'sdk'. Reading
// runmode rather than the column is not a shortcut around that, it is the only
// thing available: the ratchet lives on a conversations row and a probe has no
// conversation, being one (org, model) question asked before any work exists.
//
// Both transports produce the same three verdicts from the same status sets:
// what differs is where the status is read from, not what it means.
func (p *Prober) Probe(ctx context.Context, orgID string, m modelcatalog.Model) (Result, error) {
	if runmode.Current() == runmode.ModeMulti {
		return p.probeDirect(ctx, orgID, m)
	}
	return p.probeSDK(ctx, orgID, m)
}

// probeSDK spends the probe through the SDK subprocess, which is how a local
// delegation runs: SQLite mints them under the sdk runtime, so this is the
// transport whose failure a local user would otherwise first meet at dispatch.
//
// It resolves no credentials itself. agentproc.Run owns that resolution — the
// same call, with the same seams, that a local run makes — so a probe cannot
// test a credential path a run would not take. That is also why the ambient
// case works at all: there is nothing for this process to read, and the
// subprocess authenticates from the environment it inherits.
//
// This probe is the expensive one, and by a wide margin. probeMaxTokens has no
// equivalent here: the runtime composes its own request, so what goes on the
// wire is Claude Code's whole system prompt and tool schemas — tens of
// thousands of input tokens, and a second smaller call after the turn — where
// the direct path sends one user row and caps the completion at a token. The
// answer is worth it, because it is the only way to ask the question of a
// subscription at all, but it is why nothing may spend one of these without a
// person asking first.
func (p *Prober) probeSDK(ctx context.Context, orgID string, m modelcatalog.Model) (Result, error) {
	callCtx, cancel := context.WithTimeout(ctx, probeSDKTimeout)
	defer cancel()

	startedAt := time.Now().UTC()
	usage := &agentproc.UsageSink{}
	outcome, err := runSDK(callCtx, agentproc.RunOptions{
		Model:    m.Key,
		Message:  probeMessage,
		MaxTurns: probeMaxTurns,
		OrgID:    orgID,
		// No AllowedTools: an empty allowlist auto-denies every tool in the
		// headless posture, which is what makes "ping" a single API call and
		// nothing else.
		Secrets:     p.secrets,
		LLMResolver: p.resolver,
	}, usage)
	p.recordSDK(ctx, orgID, m.Key, startedAt, outcome, usage)

	if err != nil && IsSetupGap(err) {
		// Same contract as the direct path: a credential the runtime could not
		// assemble is a setup gap, not a verdict about the model.
		return Result{}, err
	}
	verdict, detail := classifySDK(callCtx, outcome, err)
	if verdict == VerdictGreen {
		return Result{Verdict: VerdictGreen}, nil
	}
	return Result{Verdict: verdict, Detail: detail}, nil
}

// probeDirect spends the probe from this process, against credentials it
// resolves and assembles itself — the way a multi delegation and a multi system
// job both spend one. The native engine's loop runs here rather than in the
// jail, so its cell is built with no model channel at all and this client is
// the whole of how a multi run reaches a provider.
func (p *Prober) probeDirect(ctx context.Context, orgID string, m modelcatalog.Model) (Result, error) {
	creds, err := p.resolveCredentials(ctx, orgID, m.Key)
	if err != nil {
		return Result{}, err
	}
	// The whitelist names exactly the model under test. bifrost reads an empty
	// whitelist as "no models", and a key allowed to serve something else
	// would let a probe pass on a model nobody asked about.
	pc, err := inference.ProviderCredentialsFromEnv(creds, []string{m.Key})
	if err != nil {
		return Result{}, fmt.Errorf("modelprobe: %w", err)
	}
	if got := string(pc.Provider); got != m.Provider {
		// Resolution is keyed by the model, so this cannot happen from
		// configuration alone — it would mean the resolver and the catalog
		// disagree about which provider serves this key, and probing the wrong
		// one would record a verdict about a credential path nobody asked
		// about.
		return Result{}, fmt.Errorf("modelprobe: %s resolved %s credentials but the catalog serves it through %s",
			m.Key, got, m.Provider)
	}

	account, err := inference.NewAccount(pc)
	if err != nil {
		return Result{}, fmt.Errorf("modelprobe: %w", err)
	}
	client, err := inference.New(account)
	if err != nil {
		return Result{}, fmt.Errorf("modelprobe: %w", err)
	}
	// Released after the call, on its own goroutine: shutting bifrost down
	// waits for its in-flight worker, and on a timed-out probe that worker is
	// still parked in a read only the provider or the transport can end.
	defer func() { go client.Close() }()

	callCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	startedAt := time.Now().UTC()
	completion, callErr := client.Stream(callCtx, inference.Request{
		Provider: pc.Provider,
		// The exact catalog key, never a configured override: the whole
		// advantage of a probe over a control-plane lookup is that it tests
		// the string a run will actually send.
		Model: m.Key,
		// One user row and no system prompt. A system prefix would earn a
		// cache write on tokens no later request can match, and the probe has
		// nothing to instruct.
		Rows:                          []domain.Message{{Role: "user", Content: probeMessage}},
		MaxTokens:                     probeMaxTokens,
		NoConversationCacheBreakpoint: true,
	})
	p.record(ctx, orgID, m.Key, startedAt, completion, callErr)

	// The classifier is handed the REQUEST's ctx, which is derived from the
	// caller's — so a probe abandoned mid-sweep and one that ran out its own
	// timeout are both inconclusive, rather than being read as whatever the
	// transport happened to report on the way down.
	verdict, detail := classify(callCtx, callErr)
	if verdict == VerdictGreen {
		return Result{Verdict: VerdictGreen}, nil
	}
	return Result{Verdict: verdict, Detail: detail}, nil
}

// resolveCredentials resolves the org's LLM env map for this model: through the
// role-aware seam when one is wired (minting for a role-mode Bedrock org),
// else from the secret store. Both are keyed by the model, so what comes back
// is the material for THIS model's provider and not whichever the org
// configured first.
func (p *Prober) resolveCredentials(ctx context.Context, orgID, model string) (map[string]string, error) {
	if p.resolver != nil {
		return p.resolver(ctx, orgID, model)
	}
	return agentproc.ResolveCredentialsForBundle(ctx, p.secrets, orgID, model)
}

// record lands one system_llm_runs row per probe under job=probe, so a probe
// appears under system_overhead in usage like every other call TF makes for
// itself. A successful one is tiny — a couple of tokens — but it is a real
// charge on the org's bill, and an unexplained charge is exactly what the
// confirm dialog exists to prevent.
//
// A call that produced no completion still gets a row, with zero tokens, zero
// cost and is_error set. That is not a missing cost stamp: a request rejected
// at the door (401/403/404) or one that never got an answer consumed nothing,
// so zero is the true figure, and the row is what lets an operator see probes
// failing at all. Recording the attempt rather than only the charge is the
// same contract the three background jobs hold.
//
// One bound worth naming: a stream that fails PART WAY through returns no
// completion, so tokens already generated are not counted here. That is a
// property of the neutral streaming layer — the native loop and the system
// jobs lose the same usage on the same failure — and for a one-token probe
// the amount at stake is a rounding error.
//
// Priced on the catalog key, which is guaranteed to carry a price: the catalog
// join drops any entry the datasheet cannot price, so a model that reached
// here has one.
func (p *Prober) record(ctx context.Context, orgID, modelKey string, startedAt time.Time, completion *inference.Completion, callErr error) {
	if p.recorder == nil {
		return
	}
	var (
		usage   systemllm.DirectUsage
		traceID string
		cost    float64
	)
	if completion != nil {
		usage = systemllm.DirectUsageFrom(completion.Usage)
		traceID = completion.ID
		priced := false
		cost, priced = inference.CostForUsage(modelKey, completion.Usage)
		if !priced {
			// Unreachable through the catalog, which drops any entry the
			// datasheet cannot price — so this fires only if the two ever
			// disagree. It is logged rather than swallowed for the same reason
			// the direct-call path logs it: a silent zero in the accounting
			// table is indistinguishable from a genuinely free call, and the
			// row would look like a working cost stamp reporting nothing.
			log.Warn("no pricing entry for probed model; recording zero cost", "org", orgID, "model", modelKey)
		}
	}
	p.recorder.RecordDirect(ctx, systemllm.Call{
		OrgID:     orgID,
		Job:       systemllm.JobProbe,
		Model:     modelKey,
		StartedAt: startedAt,
	}, traceID, usage, cost, int(time.Since(startedAt).Milliseconds()), callErr != nil)
}

// recordSDK lands the same job=probe ledger row for the subprocess transport
// that record lands for the direct one, so a probe costs the same visible
// thing in either mode.
//
// It prices nothing itself: the runtime reports what the call cost on the
// terminal result event, and the recorder reads it from there. That is the one
// real difference between the two — a direct probe is priced from TF's own
// datasheet against the catalog key, an SDK probe is priced by the runtime
// that made the request.
func (p *Prober) recordSDK(ctx context.Context, orgID, modelKey string, startedAt time.Time, outcome *agentproc.Outcome, usage *agentproc.UsageSink) {
	if p.recorder == nil {
		return
	}
	p.recorder.Record(ctx, systemllm.Call{
		OrgID:     orgID,
		Job:       systemllm.JobProbe,
		Model:     modelKey,
		StartedAt: startedAt,
	}, outcome, usage)
}

// IsSetupGap reports whether err is a Probe error meaning the org never
// connected this model's provider, as opposed to a fault in TF. The two need
// different answers from a caller — one names a thing an admin does in
// Settings, the other is a bug — and neither is a verdict about the model.
func IsSetupGap(err error) bool {
	return errors.Is(err, agentproc.ErrProviderNotConfigured) ||
		errors.Is(err, agentproc.ErrNoCredentialsConfigured) ||
		errors.Is(err, inference.ErrNoCredentials)
}
