# Model selection, providers, and cost — requirements

Everything user-facing in Triage Factory speaks three words: `haiku`,
`sonnet`, `opus`. That vocabulary has to become "any model any supported
provider can serve," with the spend controls a multi-tenant deployment
needs.

Status: **requirements settled — design and ticketing next.** Every open
question in §4 has been answered (struck through with its resolution
inline; the reasoning is kept, not deleted). §1 and §2 are stable to build
against; §3 records the shape the discussion converged on and remains
provisional until the design pass. **Amended 2026-08-21**: §3's
file-is-the-allowlist shape is superseded by the registry design in §3's
dated amendment block; D4 carries a matching dated note on benchmark
scores. The reasoning that was superseded is struck, not deleted, per this
document's convention. **Amended 2026-08-24**: the SDK path decouples from
the catalog — §7's shared-vocabulary rows and R12 are superseded; see §7's
dated amendment block.

Scope note: multi-mode is where the new machinery lands. **Local mode stays
SDK-bound** — see §2.6, which is a requirement, not an implementation
detail.

---

## 0. Why now

The native loop passes a conversation's stored `Model` straight to the
provider. That value is `"sonnet"` — a Claude Code CLI alias. Claude Code
resolves it; the raw Messages API the native loop uses does not, and
bifrost performs no alias expansion, so the alias goes on the wire as-is.

The same string is absent from the pricing table, so every assistant row
it produced would persist unpriced — this half is verified:

```
model=sonnet          priced=false  cost=0
model=claude-sonnet-5 priced=true   cost=0.012
```

Both halves fall out of one missing step, and patching that step alone
would re-encode the assumption that the product is Anthropic-only.

Anthropic's Fable tier is the proof that the vocabulary was never a
hierarchy — it was three product names that happened to be orderable. A
fourth arriving *below* the floor breaks any design that stored ordinal
position, and a fifth arriving between two existing ones breaks it again.

---

## 1. Requirements

### 1.1 Selection and configuration

- **R1.** A user can choose which model runs each step of a blueprint.
  Unset means the team default.
- **R2.** A team has a default model. An org has a default a team inherits
  when it sets none. With R4 resolved (seeds ship unset), the team/org
  default is the only thing every unset step falls back to — so it must be
  a **concrete, human-chosen model**, the setup flow must require the
  choice, and **TF ships no product-wide fallback model anywhere**.
- **R3.** Taskless run sessions choose a model at conversation start.
- **R4. Resolved: shipped blueprint seeds select no model at all.** Every
  seed provisions with `Model: ""` (inherit the team default). They cannot
  name a concrete id — the seed goes to every org, whatever its providers —
  and we decline to invent a vendor-neutral vocabulary to name anything
  else (see D4 for why). Accepted cost: the aggregate-style steps that
  today pin Haiku ("cheap step, don't burn the default") run at the team
  default instead; the mitigation is a UX nudge suggesting a cheap pin on
  such steps, not a shipped selection.
- **R5.** A model that cannot run the work must not be selectable. The
  agent loop is tool-driven, so `supports_function_calling` is a hard gate;
  so is a context window large enough to hold a delegation's task context
  (**≥64k input tokens**). Both gates apply to *delegated runs*. System
  jobs (§1.3, R11) are gated separately — they are toolless and
  short-context.
- **R6.** Selection is resolved **once, at dispatch**, and recorded. No
  fallback to a different model or provider at any point, including retry.
  Substituting silently would make the transcript a lie about what
  produced it and the ledger a lie about what was bought
  (`internal/agentloop/stream.go` already states this for retry; it becomes
  a system-wide invariant).

### 1.2 Caps are two different systems

Conflating these produces a design that does neither well. They are stored
differently, enforced at different times, and fail differently.

- **R7. Rate cap** — "no model costing more than $X per million tokens"
  (basis: **output price**, the headline axis — no blend). Enforced at
  **selection** time; its effect is a smaller picker. Org sets one; a team
  may set a stricter one. The effective cap is the tighter of the two.
  - When a team's default model exceeds the effective cap, **refuse and
    require an explicit choice**. Do not auto-clamp: with a single vendor
    clamping meant "drop a tier," but across vendors "the best model under
    $X" can silently change vendor, which R6 forbids.
  - **Tightening a cap over existing selections resolves by blast
    radius** — a cap that grandfathers everything is a lying control (the
    admin believes spend is capped while existing pins burn indefinitely):
    - *Defaults and the background-jobs knob*: the cap save is **refused**
      until they are re-picked under the new cap. An over-cap team default
      would break every unset step, and the admin tightening the cap is
      the right person to re-pick, at that moment.
    - *Step pins*: the cap save lists every affected pin and requires an
      explicit confirm ("N pinned steps exceed this cap and will stop
      dispatching until re-pinned"); after confirm, over-cap pins **fail
      at dispatch**, loudly naming the cap — the same failure family as
      R14, and a fail rather than a park, because no rollover fixes it
      and only a human re-pin can. Over-cap pins are badged in the
      blueprint editor. Nothing ever substitutes (R6).
- **R8. Budget cap** — "no more than $Y per period," per provider and
  optionally per model. Enforced at **runtime**; its effect is that work
  stops. Breach behavior is settled (Q3): **park** — the workspace and the
  step's transcript survive via the existing snapshot machinery; a
  mid-flight blueprint halts entirely at the breach point; **no
  auto-resume** at period rollover, waking parked work is a human action;
  and the park names its cause.
- **R9.** Budget caps must be enforceable **before** the spend, not merely
  observed after. A pre-flight check refuses to start; an in-flight check
  parks. (`spendGuard` in the native loop is the existing hook.)
  Enforcement points follow **settlement**, which is runtime-shaped — the
  native runtime settles per provider call and can park mid-run; the SDK
  runtime settles at run completion and enforces only at dispatch
  boundaries. Estimates never enforce. Full statement in §7.1.

### 1.3 Providers and access

- **R10.** An org can enable **several** inference providers at once, not
  one at a time.
- **R11.** The three system jobs — scorer, project classifier, repo
  profiler — are first-class model consumers. They are org-configurable and
  go through the same provider machinery. Configuration granularity is
  settled (Q6): **one knob** — a single org-level "background jobs model"
  covering all three, chosen by the org admin during setup. Today they are
  hardcoded to Haiku; their spend **is** recorded (`system_llm_runs`) and
  reported in usage as its own `system_overhead` category — what is
  missing is the rest: they are outside every cap, not org-configurable,
  and absent from the per-model/provider breakdown R15 requires.
- **R12.** ~~An org admin can restrict which of the org's providers a given
  team may spend against.~~ **Superseded 2026-08-24**: the team-level
  control is the model enable-set — a provider restriction is unchecking
  that path's models — so no coarser provider control stands beside it to
  disagree with. The shipped `allowed_providers` restriction retires with
  the enable-set work. See §7's amendment.
- **R13.** The model picker shows only what the org's credential can
  actually invoke. **No admin writes a configuration file describing their
  own infrastructure.** Discovery is TF's job. See §2.4 — this is harder
  than it sounds and is not uniform across vendors.
- **R14.** A model that becomes unreachable — deprecated upstream, access
  revoked, deployment deleted — must fail with a message naming the cause,
  and must be caught at save time where possible. Includes the case of a
  parked conversation whose model disappeared while it waited.

### 1.4 Cost and accounting

- **R15.** Org, team, and user cost views break down by **model and
  provider**.
- **R16.** The ledger records the provider and model id that **actually
  served** the request, not the one requested. Where the two can differ
  (an Azure deployment fronting a model, §2.4), both are recorded.
- **R17.** Historical cost never changes. Prices are snapshotted at the
  time of the run, never recomputed from the live table — the pricing
  refresh job runs daily and would otherwise silently rewrite financial
  history every night.
- **R18.** In-flight runs show a live cost estimate in the UI, computed
  from the same pricing table both runtimes use.
  - In **local/SDK mode** this estimate is display-only: not persisted per
    message, and the SDK's reported usage is authoritative once a run
    settles. The estimate must read as an estimate (the `~` prefix in
    `internal/agentmeta/footer.go` is the existing convention).

### 1.5 Deferred, deliberately

- **D1. Per-message model switching** (switching Claude→GPT inside one
  conversation) is **not v1**. It is not primarily a cache-invalidation
  problem:
  - Assistant rows carry `ReasoningDetails` with Anthropic's
    cryptographic **signatures**, which `internal/inference/assembly.go`
    replays on every subsequent turn. Those cannot be handed to another
    vendor, and it is not established that they can be handed *back* to
    Anthropic after a foreign turn intervened. Tool-call id formats differ
    too. The requirement this creates is *"the transcript stays assemblable
    across a provider switch,"* which constrains the message schema.
  - The sealed per-claim credential bundle carries **one** LLM env map
    (`bundle.LLM`, `internal/credprovision`). Per-message switching means
    either the bundle carries every enabled provider's material — widening
    what one compromised sidecar holds — or claims re-seal mid-conversation.
    Both are real work; neither is free.
- **D2. Multimodal** is not a gate. When it lands, models without it stay
  selectable and image/file upload is disabled for them.
- **D3. Parallel tool calls** are not a requirement at all. `engine.go`
  dispatches tool calls *"serially, in call order."* A model that cannot
  batch simply returns one at a time.
- **D4. Auto-resolving model selection ("pick for me under $X") is
  deferred indefinitely — a step is unset or pinned.** Any auto-selection
  needs a defensible ordering over the allowlist, and every candidate
  ordering was examined and rejected:
  - *Price as capability proxy* — a hidden assumption we declined to adopt
    as a product judgment.
  - *External coding benchmarks* (SWE-bench family, Terminal-Bench, Aider,
    LiveCodeBench, arena Elo) — fail on allowlist coverage, id mapping,
    freshness, and fatally on scaffold-dependence: an agentic score is a
    property of the *(model, harness)* pair, and a number earned in someone
    else's harness does not transfer to TF's loop.
  - *A first-party benchmark suite* — the only technically sound option,
    and rejected on cost grounds: it is real, ongoing, easy-to-get-wrong
    work that is not this product's job.
  - *Fleet outcome telemetry* (cost per merged PR) — confounded beyond
    repair for ranking: a merge is a product of the ticket author, the
    reviewers, and the implementer, any of whom may be other models or
    entirely outside TF. Attribution to the diff-writing model is bad
    causal inference presented as data.

  Until a defensible ordering exists — plausibly never, plausibly when the
  industry standardizes one — auto-selection would be false confidence
  with an audit trail. The org/team **rate cap** (R7) already provides the
  spend guardrail without pretending to know which model is best.

  *Amended 2026-08-21.* Storing and displaying a TF-harness benchmark
  score is no longer rejected — see the §3 amendment's evaluation
  contract. Two of the objections above are answered by that design: a
  score earned in TF's own harness has no scaffold-dependence problem (it
  measures exactly the *(model, harness)* pair that will run), and the
  first-party-suite cost objection is accepted knowingly by placing the
  whole apparatus — suite, runner, statistics, leaderboard — **outside the
  product repo**, with only a vendored result snapshot crossing in. What
  D4 continues to forbid is unchanged and now needs stating more sharply,
  because a stored float will attract this feature request forever:
  **no ordering the picker by any score, and no auto-resolving selection
  of any kind.** A score may be displayed, badged, and used to derive a
  categorical verdict; a step remains unset or pinned.

---

## 2. Constraints discovered

Numbers below are from the committed
`internal/inference/pricing_datasheet.json`; §6 has the commands.

### 2.1 Model ids are opaque strings. Never parse them.

Every separator is in use, and the same character means different things
in different places:

| char | keys | role |
|------|------|------|
| `-`  | 2365 | words and versions |
| `/`  | 1979 | path, 0–4 segments deep |
| `.`  | 1005 | version *and* region/vendor prefix |
| `_`  |  661 | provider labels, vendor names |
| `:`  |  211 | Bedrock version suffix |
| `@`  |   67 | Vertex date pin |
| `*`  |    4 | wildcards |

283 keys carry uppercase. None carry whitespace.

Claude Sonnet 4.5 is spelled **23 ways across 12 provider labels**:

```
claude-sonnet-4-5                                 anthropic (undated)
claude-sonnet-4-5-20250929                        anthropic (dated)
anthropic.claude-sonnet-4-5-20250929-v1:0         bedrock_converse
us.anthropic.claude-sonnet-4-5-20250929-v1:0      bedrock_converse (inference profile)
bedrock/us-gov-west-1/anthropic.claude-...-v1:0   bedrock (region in path)
vertex_ai/claude-sonnet-4-5@20250929              vertex
github_copilot/claude-sonnet-4.5                  ← dot, not dash
openrouter/anthropic/claude-sonnet-4.5            ← dot again
azure_ai/claude-sonnet-4-5                        azure_ai
```

`claude-sonnet-4-5` and `claude-sonnet-4.5` are the same model.
`claude-sonnet-4-5` and `claude-sonnet-4-5-20250929` are an alias and a
pin. **Nothing in the string tells you which relationship you are looking
at.** Any grouping ("these are all Sonnet 4.5") is editorial metadata TF
adds, never derived.

Corollary: **dual access is the normal case, not an edge case.** An org
with both Anthropic and Bedrock has ~9 spellings of every Claude model
available at once, at genuinely different prices — Bedrock entries carry
region multipliers (`provider_specific_entry: {"us": 1.1}`, currently
unapplied by `computeTextCost`).

### 2.2 The catalog already exists and is richer than assumed

`internal/inference/pricing_datasheet.json` is not a pricing table. It is a
**2,406-model multi-vendor catalog**, a filtered mirror of LiteLLM's
`model_prices_and_context_window.json`, refreshed daily by
`.github/workflows/refresh-pricing.yml`. Per model it carries vendor
(`litellm_provider`), the full four-way price breakdown including cache
tiers, context limits, and ~20 capability flags.

What it does **not** carry: display names, or any tier/capability ranking.

**`scripts/refresh-pricing.sh` rewrites the file wholesale** — fetch,
filter, write, key-sorted and deterministic, *"It writes ONLY data."* So
TF-authored fields cannot live inside it; they would be wiped on the next
run. TF's editorial content needs a file TF owns, joined at boot.

### 2.3 Exclusion is derived, not curated

TF must not hand-maintain price data. It is a silent fork of upstream that
rots, and the failure mode is invisible mis-billing. Where a vendor
publishes numbers LiteLLM lacks, the fix is a PR upstream. Where nobody
publishes them, the model cannot be priced and must not be offered.

Rules the refresh script can enforce with no human judgement:

| rule | drops (of the 1,366 tool+64k set) |
|------|------|
| must have `input_cost_per_token` and `output_cost_per_token` | **47** (20 `github_copilot`, 15 `dashscope`) |
| if `supports_prompt_caching`, must have ≥ `cache_read_input_token_cost` | **48** |
| must have `max_input_tokens` ≥ 64k — absent ⇒ **fail closed** | 36 with no window at all, plus small windows |
| `mode: chat` and `supports_function_calling` | 2406 → 1547 |
| `litellm_provider` must map to a bifrost provider | — |

Two findings that shaped these:

- **A nil rate is priced as `0`.** `tieredRate` returns 0 for a missing
  price, so an unpriceable model currently bills as free rather than
  failing. That is why the first two rules exclude rather than approximate.
- **A missing cache-*write* price is usually correct.** 282 of 330 such
  models have a cache-*read* price but no write price — the signature of a
  vendor with automatic caching (OpenAI, Gemini, xAI, Azure charge nothing
  to populate a cache). **Zero Claude models are affected.** The 7 OpenAI
  entries that claim caching with no cache prices at all are legacy GPT-4 /
  early-4o, which predate OpenAI's caching entirely — an upstream data
  error, and free to drop.

The 36 models with no context window are entirely `replicate` (21) and
`together_ai` (15) — re-hosting aggregators. Every model among them is also
available first-party with correct metadata, so failing closed costs
nothing. One of them, `replicateopenai/gpt-oss-20b`, is a missing-slash
typo in upstream data — evidence that a refresh gate catches real defects.

### 2.4 Entitlement is not uniform across providers

R13 says the picker shows only what the credential can invoke. That is
three different problems.

- **Anthropic** — `/v1/models` reflects the key's access. Behaves as
  expected.
- **Bedrock** — bifrost's `ListModels` calls
  `https://bedrock.{region}.amazonaws.com/foundation-models`
  (`ListFoundationModels`): **every model available in the region, not the
  ones the account has been granted.** Model access is a separate grant
  and none of the filters bifrost passes (customization type, inference
  type, output modality, provider) touch entitlement. A newly released
  model will appear in the picker and fail at dispatch with
  `AccessDeniedException`.
  - Verified against the generated SDK (`aws-sdk-go-v2/service/bedrock`
    v1.66.1): `ListFoundationModelsInput` offers exactly four filters —
    `ByCustomizationType`, `ByInferenceType`, `ByOutputModality`,
    `ByProvider` — and `FoundationModelSummary` carries no entitlement
    field at all. Its only status is `ModelLifecycle` (available vs.
    deprecated). There is no filter and no field that reflects account
    grants.
  - Second wrinkle: `ListFoundationModels` returns **base model ids**, not
    **inference-profile ids** (`us.anthropic.…`), and newer Claude models
    on Bedrock can only be invoked through a profile. `ListInferenceProfiles`
    is a separate operation. The ids you can list and the ids you can call
    are different sets.
  - A control-plane entitlement API does exist, and it does not help as
    much as it sounds:

    ```go
    // aws-sdk-go-v2/service/bedrock v1.66.1
    GetFoundationModelAvailabilityInput{ ModelId *string }   // ONE model
    GetFoundationModelAvailabilityOutput{
        AgreementAvailability   *types.AgreementAvailability // {Status, ErrorMessage}
        AuthorizationStatus     types.AuthorizationStatus    // AUTHORIZED | NOT_AUTHORIZED
        EntitlementAvailability types.EntitlementAvailability// AVAILABLE  | NOT_AVAILABLE
        RegionAvailability      types.RegionAvailability     // AVAILABLE  | NOT_AVAILABLE
    }
    ```

    It is **per-model — there is no batch or list variant**. The agreement
    operations are `CreateFoundationModelAgreement`,
    `DeleteFoundationModelAgreement`, and
    `ListFoundationModelAgreementOffers`; note the last lists what is *on
    offer*, not what the account *holds*. **No operation answers "which
    models am I entitled to."**

    So checking N models costs N calls either way, and the control-plane
    route additionally needs its own IAM action on a different service
    endpoint — asking every admin to widen the assumed role, which is R13's
    config-file problem in another form. It also answers a weaker question
    (an agreement record exists across four independent axes) than a probe
    (this credential can invoke this id right now).
- **Azure** — enablement **is** a deployment, listable on the data plane
  with the same key. But the selectable unit is a deployment whose name is
  admin-chosen and arbitrary (`gpt5-prod`), which appears in no catalog and
  has no price of its own. Only the deployment's underlying model joins to
  pricing.

**The uniform mechanism is a probe**: one minimal request per candidate
model, cached. It requires no permission the run does not already need —
if the probe fails, the run would have failed too — where every
control-plane option asks the admin to grant something extra so TF can ask
a question the existing credential could answer. It costs the same number
of calls as Bedrock's per-model entitlement check, works identically for
every vendor instead of needing three integrations, and tests the exact id
that will be invoked, which sidesteps the Bedrock profile mismatch
entirely.

Probe requirements (semantics settled by Q4): bound the candidate set to
the allowlist ∩ `ListModels`; persist per `(org, provider, model)`
(`poll_readiness` is the precedent for a durable org-scoped readiness
table); **once green, never re-probe automatically** — one successful
connection in a row's history satisfies the check permanently, with a
per-row "test connection" affordance for manual re-verification (no TTL;
late drop-outs surface at dispatch per R14); and **distinguish refusal
from failure** — a 403 is entitlement and counts as a red result, a 500
or a timeout is neither green nor red, or one bad minute would block a
save on a model that works.

### 2.5 bifrost already has the provider-shape indirection

```go
type AliasConfig struct {
    ModelID   string   // wire model identifier sent to the provider
    ModelName *string  // canonical model name used for pricing, logging
    Region    *SecretVar
    ProjectID *SecretVar
    *AzureAliasCfg     // per-alias Endpoint override
    *BedrockAliasCfg
    *VertexAliasCfg
}
type KeyAliases map[string]AliasConfig   // schemas.Key.Aliases
```

`ModelID` is what goes on the wire — the Azure deployment name, the
Bedrock inference-profile id. `ModelName` is the canonical id *used for
pricing* — the catalog key. Region and project are per-alias.

**Ownership: bifrost resolves, TF supplies.** This is worth stating flatly,
because "bifrost has an alias mechanism" is easy to hear as "bifrost
maintains the aliases," and it does not.

| bifrost provides | TF must provide |
|---|---|
| the `AliasConfig` shape and its per-provider sub-configs | the map itself, on `schemas.Key.Aliases` via `Account.GetKeysForProvider` |
| `ResolveConfig` at request time, and wire substitution via `req.SetModel` | **discovery** — listing Azure deployments and Bedrock inference profiles. `ListModels` reports ids; nothing in bifrost authors an alias entry |
| alias-aware `ListModels` (a listed model can surface under its alias key) | the canonical `ModelName` for each entry — i.e. the catalog join |
| the resolved alias on the request context | persistence, and rebuilding the map on every call |

Every `Aliases:` assignment in bifrost's source populates a
`ListModelsPipeline` *from* `key.Aliases`. The map is read-only input.

**TF does not currently supply one.** `buildKey` in
`internal/inference/account.go` populates `ID / Name / Value / Models /
Weight` and stops, never setting `Aliases`. Wiring it is the one plumbing
gap on the inference side.

Note the lifecycle constraint: `EnvCredentials.ForCall` rebuilds the
account and client **on every provider call** — deliberately, so an
expiring STS triple can never outlive the credential that minted it. So
there is no bifrost-side registry that accumulates aliases across calls.
The table is constructed fresh from TF's own storage each time, which is
also why TF is the only durable holder of the alias→`ModelName` mapping.

The payoff is still real: per-vendor id weirdness lands in one table
instead of leaking into config, pricing, and the picker separately, and TF
requests alias names uniformly at the call site.

**`ResolveConfig` is a pure in-memory map lookup** — exact key first, then
a case-insensitive scan; ~14 lines, no I/O of any kind. And the airgap
question generalizes: **embedded bifrost core makes no runtime metadata
fetches at all.** Verified against the pinned fork: no embedded datasheet,
no `go:embed` anywhere, and the only HTTP outside the provider API calls
themselves is `FetchAndEncodeURL` — inlining a remote image/document for
providers whose surface only accepts bytes, invoked only when a request
carries a remote URL, SSRF-hardened. The model-params cache
(`providers/utils/modelparamscache.go`) has a *registerable* miss handler —
the hook the standalone bifrost gateway presumably wires to its own
datasheet service — but TF never registers one, so on a miss embedded core
falls back to a compiled-in static table of Anthropic max-output values.
Model metadata in TF comes from the committed datasheet snapshot and TF's
own tables, full stop — nothing reaches out at runtime, which is the
posture airgapped and security-sensitive deployments require. (The
standalone bifrost *service* may well pull model config from upstream at
runtime; TF embeds core, which does not.)

**What bifrost reports back** (`bifrost.go`, the alias arm of the streaming
request path): on a match it sets `resolvedModel = aliasConfig.ModelID` and
calls `req.SetModel(resolvedModel)` — so **`ModelID` is what goes on the
wire**. It then stamps the response via

```go
result.PopulateExtraFields(requestType, provider,
    originalModelRequested,   // → ExtraFields.OriginalModelRequested (the alias name)
    attemptResolvedModel)     // → ExtraFields.ResolvedModelUsed      (ModelID)
```

so the answer is **neither**: `ModelName` populates no response field.
The full `AliasConfig` is available on the context under
`BifrostContextKeyResolvedAlias`, which is where bifrost's own pricing
layer reads it.

Note also that `reassembleStream` currently captures `resp.Model` — the
*provider's* echo, not bifrost's extra fields — and for Azure that is the
underlying model rather than the deployment, so the two disagree by
provider.

**Consequence: price off TF's own alias table, keyed by the alias name TF
requested.** TF builds the table, so it holds the canonical `ModelName`
without a round trip, and it is the only source that means the same thing
across every provider. Do not derive the pricing key from the response.

### 2.6 Local mode stays SDK-bound

`internal/agentproc/credentials.go` returns an **empty env map** in local
mode when no Anthropic/Bedrock secret is configured, and the subprocess
inherits the host environment — the *"Claude Code subscription handles
auth"* flow. Those users authenticate by OAuth and **have no API key at
all**.

Bifrost is a raw Messages API client and cannot ride a subscription.
Routing local-mode work through it would convert every subscription user
into "you must now buy API credits," experienced as the product breaking
after an upgrade.

This is why `internal/agentprompt`'s `nativeMachinistBlocks` refuses any
mode but multi. That refusal is load-bearing.

The migration's one load-bearing assumption about this path is verified
(2026-07-29, CLI 2.1.220, subscription OAuth with no API key present):
`claude -p --model claude-sonnet-5` and `--model
claude-haiku-4-5-20251001` both work — concrete ids, dated and undated,
are CLI-valid on subscription auth — and an invalid id fails with a
legible error naming the model, not a hang.

`internal/systemllm` already has the split R11 needs: direct API calls in
multi mode, SDK subprocess in local. Only the multi-mode half changes.

*Amended 2026-08-24.* The CLI verification above (concrete ids are valid
on subscription auth) stands as a fact but is no longer load-bearing: the
SDK path offers and stores its own alias vocabulary rather than the
catalog's concrete ids. See §7's amendment.

### 2.7 Things that do not port across vendors

- **Reasoning effort.** `Spec.Effort` maps to Anthropic budget-tokens or
  adaptive thinking; OpenAI's `reasoning_effort` has different values and
  different meaning; many models ignore it. "medium" is not a portable
  unit.
- **Prompt caching economics.** 526 of the 1,366 gated models support it.
  Without it, TF's entire cacheable-prefix design — `agentprompt.Build`'s
  byte-identical guarantee, the breakpoints in
  `internal/inference/cachecontrol.go` — buys nothing, and the same run
  costs several times more. This is not a gate, but the user must see it
  before choosing.
- **Gemini has two caching modes, and only one fits TF's shape.**
  *Implicit* caching is the analog of what TF already relies on everywhere:
  the provider notices a repeated prefix on its own, discounts the cached
  tokens, reports them in usage — no management, no storage fee,
  `computeTextCost` prices it through the normal cache-read rate.
  *Explicit* caching is a different product: you create a cache object over
  a chosen context with a TTL, pay a **per-token-per-hour storage fee** (a
  dimension `computeTextCost` has no slot for), and reference the cache id
  in each request. It exists for a large *static* corpus reused across many
  independent requests. TF's cacheable prefix is per-conversation and
  varies per run (mission, task context), which is exactly the shape
  implicit caching serves — so explicit caching is **not wanted now**. It
  would become interesting only if something like a project's knowledge
  base were ever held in-context wholesale across many turns; if that day
  comes, it is new cost machinery, not a config flag.

---

## 3. Shape converged on so far (provisional)

Recorded so the requirements have something concrete to be argued against.
Not accepted.

1. **Two files, joined at boot.**
   - `pricing_datasheet.json` — auto-refreshed upstream mirror. Never
     hand-edited.
   - A TF-owned file of supported entries, each with the display name
     exactly as it should render. ~~**This file is the allowlist**: in it ⇒
     supported and named; absent ⇒ not offered. One concept, not two.~~
     **Superseded 2026-08-21** — three concepts after all, and only one of
     them belongs to the file; see the amendment below. The file is a
     registry of editorial facts; the allowlist is derived.
2. **CI on the refresh job** distinguishes blocking from reporting:
   - *Block*: an entry we name no longer exists upstream (retirement needs
     a decision); an entry we name now fails a §2.3 gate; a
     `litellm_provider` appears that maps to no bifrost provider and is not
     in a known-unsupported list.
   - *Report in the PR body*: catalog entries passing every gate, under a
     provider TF supports, that TF has not named. That is the "GPT-5.2
     shipped" signal without the build going red because Azure added 40
     SKUs. This should explicitly separate _new_ omissions vs _standing_
     omissions that existed before the most recent pull as well.
3. **No alias→id resolution layer.** The only reason one would exist is
   rows already in databases (§5), which is a one-time migration. A runtime
   alias map has drift built in: resolve `"sonnet"` at call time and every
   historical row storing it gets retroactively repriced when the map
   moves.
4. **The picker is** ~~`named allowlist ∩ ListModels ∩ probe-verified`~~
   `derived eligibility ∩ org enablement ∩ ListModels ∩ probe-verified`
   (amended 2026-08-21 — the first term changed with item 1; the registry
   names and groups that universe rather than bounding it), with Azure's
   ListModels term being deployments-mapped-to-models.
5. **Per-org alias tables** (§2.5) hold the per-vendor id shapes; TF
   requests alias names.
6. **Rate caps shrink the picker; budget caps stop work.** Separate
   storage, separate enforcement points.

### Amendment (2026-08-21): the file is a registry, not the allowlist

Item 1's "one concept, not two" was wrong in the other direction: the
file's single bit was carrying **three** different claims, and only one of
them is irreducibly human.

1. **Capability** — TF can drive and bill the model. Fully derivable, and
   §2.3 already demanded exactly that ("exclusion is derived, not
   curated"): the mechanical gates plus the provider map compute it with
   no judgement. Hand-listing capable models re-curates what §2.3 derives.
2. **Quality** — TF has evaluated the model in its own harness. Editorial,
   but it is a *statement about* a model, not a reason to withhold it.
3. **Identity** — what the model is called, which datasheet keys are
   spellings of it, and which harness family drives it. The only part
   that cannot be derived (§2.1: names and groupings are editorial
   metadata TF adds, never parsed out of keys).

Binding the first two to the file makes TF the blocking factor for models
its providers already serve. The credential is the customer's own
subscription — their Bedrock account, their spend, not a repackaging — so
when a vendor ships a new model there, waiting on a TF release that edits
a JSON file gates something TF neither executes nor pays for. The only
legitimate TF-side gates are *the harness can drive it* and *the ledger
can price it*, and both are mechanical.

**The registry.** One row per **logical model** — not per spelling:

```json
{
  "name": "Claude Sonnet 5",
  "family": "claude",
  "keys": {
    "anthropic": "claude-sonnet-5",
    "bedrock": "us.anthropic.claude-sonnet-5-...-v1:0"
  }
}
```

The name is written once; the "(via Bedrock)" parenthetical is derived
from which key matched instead of hand-written per spelling; file order is
display order. The word "Bedrock" itself comes from a TF-authored display
name per supported *access-path* provider — the provider map's value
vocabulary, which is also the registry's `keys` vocabulary — never from
upstream's labels, which split one path into several spellings
(`bedrock`/`bedrock_converse`; three `vertex_ai-*` labels are one
"Vertex AI") and whose casing is not derivable (xAI, DeepSeek, OpenAI).
It is a hand-written table of ~a dozen entries, kept beside the provider
map rather than inside it (the refresh script reads that file expecting
string values), with a completeness check — every provider the map
supports must be named — so enabling a provider forces the naming
decision the way the refresh gate forces the mapping decision. The `keys` grouping is the editorial identity mapping §2.1
promised and is **load-bearing, not decoration**: the derived universe
contains alias-and-pin duplicates (`claude-sonnet-5` *and* its dated
spelling, plus prior generations) that nothing in the strings relates, and
the registry hiding spellings it already covers is what keeps the
uncurated tier below from being noise.

**The derived universe.** What may be offered =
`datasheet ∩ §2.3 gates ∩ provider map ∩ supported families`, recomputed
from whatever snapshot the deployment holds. The eligibility logic gets a
Go implementation shared with the refresh gate so CI and runtime cannot
drift. A new model on an already-supported provider appears when the
snapshot carries it — no TF code change. TF's engineering unit is the
**execution path** (a provider integration, a harness family), never the
model.

**The uncurated tier.** Eligible-but-unnamed models surface under their
raw datasheet key, disabled by default, org-admin-enableable through the
enable-set machinery. Curated vs. uncurated travels as data on the model
read (the same pattern as `availability`), never as a gate: a model TF has
not evaluated is *ugly and opt-in*, not absent. People who want a day-one
Bedrock model know exactly what its raw key means.

**Family.** An editorial registry field, and the one legitimate hard gate:
a family the harness genuinely cannot drive is not offered. It is
deliberately *assigned*, never parsed from keys — which dissolves the
otherwise-unsolvable problem that family is not derivable from opaque
ids. The harness work a family selects splits into three buckets:

- *Prompt text* — the `agentprompt` manifest's existing family axis.
- *Tool surface* — which tools, and which shapes (find-and-replace
  editing for Claude, patch-style for GPT), the loop presents. Needs the
  same manifest discipline prompts already have; this is new work.
- *Loop mechanics* — reasoning-signature replay, cache breakpoints,
  effort mapping: per-family code above bifrost. Bifrost normalizes the
  wire *envelope* (tool calls are JSON-schema-defined with JSON arguments
  on every major vendor; the differences are content-block vs. tool_calls
  shapes, result framing, id formats) — it cannot normalize which tools a
  model is *good at*, which is why the tool-surface bucket exists.

Family-granular is the default grain; a model-granular override is rare
and deliberate. This is what keeps maintenance from scaling with model
count: a vendor's next model in an existing family costs one registry row
(or zero, in the uncurated tier), never a harness change.

**Evaluation.** The benchmark apparatus — suite, runner, repeated runs,
statistics, leaderboard — lives **outside the product repo**, in its own
system. What crosses in is a vendored snapshot, the same relationship the
pricing datasheet has to LiteLLM: a data file with provenance, written
only by its sync. Per logical model:

```json
{
  "bench_version": "3",
  "score": 0.78,
  "runs": 40,
  "scored_at": "2026-08-14",
  "verdict": "validated",
  "scored_via": "claude-sonnet-5"
}
```

The contract that keeps every semantic on the bench side: the **verdict is
derived by the bench system before export** (the threshold is part of the
methodology, not the product); `bench_version` is opaque to the product,
which only equality-compares it against the snapshot header's current
version to detect staleness (stale ⇒ unevaluated); scores are comparable
**only within one version**, and the version covers the whole measurement
apparatus — suite *and* harness configuration — since a score is a
property of the *(model, harness)* pair; `runs` rides along because a mean
of 5 and a mean of 200 are different facts; `scored_via` records which
key actually served the runs, because score transfer across providers is
a per-provider-family policy (safe for first-party hosts serving identical
weights, unsafe for aggregators that may re-quantize). An absent snapshot
file means every model reads *unevaluated* and blocks nothing — the bench
system can arrive whenever it arrives.

This lands the product's model layer on three data files with **one
writer each**: the registry (hand-authored), the pricing snapshot (its
refresh script), the bench snapshot (its sync). Measurement data is never
hand-edited into an editorial file — that is the pricing rule (§2.3)
applied uniformly.

**Freshness, decided in order.** The formula is frozen in the binary; the
numbers are data (a release is required only when the cost *shape*
changes, which the refresh script's unknown-field tripwire detects).
Lever one: make the release train data-fast — once the refresh gates
land, the daily data PR auto-merges, and SaaS deploys on TF's own
cadence. Lever two, **deferred until a real customer lags releases**:
runtime adoption of a TF-operated, signed snapshot with the same gates
applied at adoption time (unknown cost fields ⇒ refuse, fall back to
embedded, log loudly) — never a raw third-party URL at runtime, opt-in
for self-hosted, and nonexistent for airgapped installs, which keep the
embedded posture §2.5 records. One freshness cost is real either way: for
API-key customers the provider invoices at live prices while TF's ledger
estimates from the snapshot, so staleness is ledger-vs-invoice drift, not
mis-billing (R17 already insulates history).

**What does not change.** R6 (resolve once, no fallback), the probe and
its semantics (§2.4, Q4), enable-sets as the org's control surface, the
mechanical gates failing closed, D4's prohibition on ordering and
auto-selection, and §7's one-contract mode discipline — ~~local serves the
identical read from the identical registry, and its universe stays what
the SDK can drive~~ (superseded 2026-08-24: local serves the same contract
from the SDK's own registry; §7's amendment).

**Ship order.** The four-entry Anthropic file ships now with
file-as-offered-set semantics — correct at one provider and one family,
where the derived universe would add only alias/pin duplicates the
registry exists to hide. The registry schema (logical models, `keys`,
`family`), the derived universe with its uncurated tier, and the
tool-surface family axis land with the multi-provider work, which is the
point where file-as-allowlist would otherwise start gating customers. The
bench snapshot lands when the external bench system exists; nothing
sequences on it.

---

## 4. Open questions

- ~~**Q1 — May shipped blueprint seeds name a cost class?**~~ **Answered:
  no.** Seeds ship with no model at all (R4). No vendor-neutral vocabulary
  exists anywhere in the product.
- ~~**Q2 — Does a blueprint step store a model or a provider-pinned
  entry?**~~ **Answered.** A step is **unset** (inherit the team default)
  or **pinned** (an explicit model named against the org's aliases —
  tenant-local allowed, since an Azure deployment alias is unavoidably so).
  There is no third, auto-resolving mode — see D4 for the full reasoning.
  Two consequences survive as requirement details:
  - A **new** pin that violates the effective rate cap fails at **save**
    time; a pre-existing pin that a *tightened* cap strands follows R7's
    blast-radius rule — confirmed by the admin at cap-save, then failing
    at dispatch naming the cap.
  - A pinned step whose model becomes unavailable fails per R14 — loudly,
    naming the cause. It never silently re-resolves.
- ~~**Q3 — Budget-cap breach behavior.**~~ **Answered: park, everything,
  no auto-resume.**
  - Breach **parks** the engagement. The snapshot machinery already exists;
    the blueprint's workspace survives, and the step's transcript survives
    with it. Nothing is thrown away.
  - **No auto-resume at period rollover.** Waking parked work is a human
    action. The admin capped spend because spend was the problem; a pile of
    agents silently springing back to life on the 1st is the opposite of
    what they asked for.
  - A blueprint mid-flight **halts entirely at the breach point** — the
    parked step stays parked and later steps never start.
  - The park must say why ("parked: org Bedrock budget reached"), or this
    ships as "agents randomly freeze near month-end."
- ~~**Q4 — Probe eagerness.**~~ **Answered: eager, sticky, and gated —
  with explicit comms.**
  - Probe on credential save, with UI copy that says plainly a tiny paid
    test call (~2 tokens) is about to run. No silent spend, however small.
  - **Once green, never re-probed automatically.** A single successful
    connection in a row's history satisfies the check permanently.
  - Every model row carries a **"test connection"** affordance for manual
    re-verification.
  - **Save is gated**: the settings field cannot be saved until every row
    has at least one green in history. Pressing save with untested rows
    pops the confirm dialog and runs the tests.
  - Accepted limitation, stated deliberately: a historically green model
    that later loses access is **not** caught eagerly — the alternative is
    re-testing every model on every save, which is rejected. Late
    drop-outs surface at dispatch per R14, loudly, naming the cause.
  - *Amended 2026-08-24*: "~2 tokens" prices the **direct** transport. The
    SDK transport spends a full harness invocation per probe, so consent
    copy states the cost of the transport the deployment uses.
- ~~**Q5 — Does bifrost report `ModelName` or `ModelID` on responses?**~~
  **Answered** (§2.5): neither. `ModelID` goes on the wire and lands in
  `ExtraFields.ResolvedModelUsed`; the alias name lands in
  `OriginalModelRequested`; `ModelName` populates no response field.
  Pricing joins off TF's own alias table, keyed by the requested alias.
- ~~**Q6 — Are system-job models org-configurable per job, or one "cheap
  model" setting?**~~ **Answered: one knob.** A single org-level
  "background jobs model" setting covering scorer, classifier, and
  profiler alike, chosen by the org admin during setup — alongside the
  R2 default-model choice, making the setup flow's model section exactly
  two picks. Per-job knobs can be added later if a real need appears;
  removing them later could not.

---

## 5. Migration surface

Every one of these holds the old three-word vocabulary:

| location | field | today |
|---|---|---|
| `settings` row | `AI.Model` | team default: `haiku\|sonnet\|opus` |
| `settings` row | `MaxLLMModelTier` | org cap, same vocabulary |
| `prompts` table | `model` | per-step override, `""` = inherit |
| `internal/server/prompts` | `allowedModelOverrides` | validation list |
| `internal/promptseed` | seeded `Model:` | ships `"haiku"` |
| `internal/ai/scorer.go` | `SystemJobModel` / `SystemJobModelDirect` | the one place that already resolves |
| `internal/ai/pricing.go` | alias rows in `pricing` | `"sonnet"` frozen at Sonnet-4.5-era prices |

The `internal/ai/pricing.go` alias rows are worth calling out: they encode
what `"sonnet"` meant when they were written. They are reached only by the
approximate (`~`-prefixed) footer path today, so they are not actively
mis-billing — but they are exactly the drift this design exists to remove,
and they collapse into the single catalog lookup.

*Amended 2026-08-24.* The compatibility baseline for every migration in
this epic is **v1.12.2** — the last cut release. Nothing merged since has
reached an install, so tickets write **target-state DDL only**: no
incremental folds between post-v1.12.2 shapes, no compat for columns that
never shipped. A single consolidated migration lands before v1.13.0 and
owns the one real upgrade path from what v1.12.2 holds (this table,
including the baseline's `max_llm_model_tier`).

---

## 6. Reproducing the numbers

```bash
python3 - <<'EOF'
import json
from collections import Counter
d = json.load(open('internal/inference/pricing_datasheet.json'))
def win(v): return v.get('max_input_tokens') or v.get('max_tokens') or 0

tools = [k for k,v in d.items()
         if v.get('mode')=='chat' and v.get('supports_function_calling')]
gate  = [k for k in tools if win(d[k]) >= 64000]
print(f"total {len(d)} | chat+tools {len(tools)} | +64k {len(gate)}")

noprice = [k for k in gate if 'input_cost_per_token' not in d[k]
                           or 'output_cost_per_token' not in d[k]]
nocache = [k for k in gate if d[k].get('supports_prompt_caching')
           and 'cache_read_input_token_cost' not in d[k]
           and 'cache_creation_input_token_cost' not in d[k]]
nowin   = [k for k in tools if not win(d[k])]
print(f"unpriceable {len(noprice)} | caching-but-unpriced {len(nocache)} | no window {len(nowin)}")
print(Counter(d[k]['litellm_provider'] for k in noprice).most_common(5))
EOF
```

Provenance for the snapshot itself is in
`internal/inference/pricing_provenance.json` (upstream repo, pinned
commit, fetch date, filter).

---

## 7. Mode split — one contract, two implementations

Local mode keeps full frontend parity with the multi-mode model surface.
The mechanism is the repo's existing precedent: like `db.Stores`, the model
surface is **one API contract with two implementations**, and the frontend
cannot tell which one it is talking to. "Mock" is not a UI branch; it is
the second, simpler implementation of the same endpoint. Mode differences
travel as **data** (an availability field, a filtered universe), never as
`if (mode)` branches in components.

Five layers, most-shared first:

| layer | multi | local |
|---|---|---|
| vocabulary & storage | concrete model ids | ~~**identical** — the SDK's `--model` accepts concrete ids, so no translation exists anywhere~~ superseded 2026-08-24 — the SDK's own alias vocabulary, stored as-is; see the amendment below |
| catalog & names | allowlist ⋈ datasheet, embedded | ~~**identical** — it ships in the binary~~ superseded 2026-08-24 — the per-SDK list ships in the binary beside the native registry; see the amendment below |
| model universe | filtered by the org's configured providers | ~~filtered by what the SDK can drive: the Claude family via Anthropic direct / Bedrock / Vertex.~~ superseded 2026-08-24 — the SDK's supported-model list, decoupled from the catalog; see the amendment below. **Not a mock — a truth.** A GPT entry in a local picker would be a lie, because nothing local can execute it |
| availability | `verified` via probe, save-gated (Q4) | **identical vocabulary** — local probes through the agent runtime, which reports the provider's HTTP status on its terminal event (`api_error_status`), so a subscription with no key this process could assemble a request from still yields a real verdict. Same four states, same test-connection button, same stored rows |
| caps & usage | full | shared ledger shape (`messages.cost_usd`); rate caps work identically (a picker filter against catalog prices); budget caps per §7.1 |

One deliberate asymmetry, written down so it is a decision rather than
drift:

- **Setup flow.** Multi requires the two model picks (R2, Q6) with no
  shipped fallback. Local pre-fills both with the migration's concrete
  equivalents of today's defaults, preserving the zero-friction first run.

Availability is **not** one of them. The probe has two transports, and they
are the two conversation runtimes' own: an in-process call over
`internal/inference` (what `agentloop` builds per call for the **native**
engine) and the **SDK** subprocess. Either way it spends the request through
the same client a delegated run does, so it tests that run's credential path
rather than a parallel one — and what it asks, and what the answer means, are
identical, so the field carries no mode difference for a client to read.

It picks by mode because the mode settles the runtime: the dialect *is* the
mode and the enqueue stamps the ratchet per dialect — Postgres mints
delegations `native`, SQLite mints them `sdk`. Reading the ratchet itself is
not an option, since it lives on a `conversations` row and a probe has no
conversation.

The one thing that genuinely differs between the modes is what an org's
credential source can be: only local can run on the host's, and that source
is what makes every provider reachable rather than only the bound ones.

### Amendment (2026-08-24): the SDK path is its own registry

The five-layer table recorded vocabulary and universe as shared, on the
verified fact that the SDK's `--model` accepts concrete ids. Superseded:
**the SDK path decouples from the catalog entirely.** The registry of the
2026-08-21 amendment is the *native* registry — bifrost wire ids joined to
the datasheet — and each supported SDK carries a **parallel
supported-model list** in its own vocabulary. For Claude Code that list is
`haiku` / `sonnet` / `opus` / `fable`: family aliases the SDK resolves
itself, against whichever access path its environment selects — the same
alias reaches Anthropic on a subscription and Bedrock under
`CLAUDE_CODE_USE_BEDROCK=1`. On this path the provider is a property of
the credential, never the id, which is the fact the shared vocabulary
could not express: it forced a local picker to offer the Anthropic and
Bedrock spellings side by side and ask the user a question the
deployment's environment had already answered.

What follows:

- **Two registries, one package.** `internal/modelcatalog` keeps its name
  and widens its job to "the models TF offers, under each execution
  vocabulary it offers them in": the native registry (the datasheet join,
  unchanged) beside per-SDK lists (`sdk_models.json`, keyed by SDK id —
  `claude-code` — TF-authored display names, **no prices**, no datasheet
  join). Not subpackages, and never `multi`/`local` in a path: the axis is
  the execution vocabulary, and mode is only what selects between them
  today. The one new seam — *what may this deployment offer or store* — is
  a single resolver taking the mode (eventually the access path) as a
  parameter, the `modelaccess.For` discipline, so validators stop meaning
  "∈ the catalog" and start meaning "∈ this deployment's universe" through
  one door.
- **One contract, universe per deployment.** `GET …/models` serves the
  native registry in multi and the SDK list in local — the mode-as-data
  mechanism this section already mandates. `prices_per_mtok` becomes
  optional on the wire; its absence is how a client knows cost is
  harness-settled.
- **A row names its executor once there is more than one.** With a second
  SDK configured, a model choice on this path also chooses the harness
  that runs the conversation, so rows carry an `sdk` field — absent on
  native rows, the same absence convention as the price — and the picker
  groups by it. Keys stay each SDK's vocabulary verbatim, with no
  cross-SDK uniqueness asserted (TF does not control those
  vocabularies), so the unambiguous identity for a stored choice is the
  `(sdk, model)` pair once a second SDK exists; today the bare alias
  stores, and the second-SDK ticket carries the widening. The likely
  home for the run-time half already exists: `conversations.runtime` is
  which engine drives the transcript, and its generic `sdk` value
  naturally becomes the harness's own id at that point — recorded here
  as the direction, decided in that ticket, since it changes stored
  ratchet strings.
- **Stored local vocabulary is the alias.** Team defaults, step pins, and
  the background-jobs knob store `sonnet` in a local install, never
  `claude-sonnet-5`. No translation layer exists in either direction (§3
  item 3 stands): local stores aliases and sends aliases, multi stores
  concrete ids and sends them, and §0's bug cannot return because nothing
  native ever reads a local row — the alias never reaches bifrost.
- **Cost on the SDK path is harness-settled — verified, not assumed.**
  `inference.CostForUsage` (the datasheet interpolation) is called only by
  the native engine, the direct probe, and `systemllm`'s direct arm; the
  SDK path carries cost as the runtime's terminal `total_cost_usd`, parsed
  once and written whole. R18 already made the SDK's reported figure
  authoritative at settle; this removes the last implication that a local
  row ever joins the datasheet.
- **Availability is a fact about a TF-owned credential — on every
  path.** A stored verdict needs a stable subject. Multi's rows are about
  the org's bound credential, and unbinding it is an event the
  `unconfigured` derivation sees; under local's system credentials the
  subject would be the host environment — mutable, unobservable, and
  invalidated by nothing TF can detect — so on that path nothing is
  stored, no badge renders, no save gate engages, and the probe surface
  does not appear in settings. The surface exists exactly when the org
  brings its own credential, which needs no mode branch: always in multi,
  and in local once the credential wiring (TFAC-888) makes BYOK real
  there — the same bit `modelaccess` already resolves. A local BYOK
  verdict keys as native ones do, `(credential family, id in the asking
  path's vocabulary)`: `(anthropic, sonnet)` records that this org's
  Anthropic key can invoke `sonnet` through the SDK. Probe candidates
  follow the universe — a local sweep is the aliases through the SDK
  transport (see Q4's amended cost note). The probe already resolves
  credentials exactly as a run does, so it follows the TFAC-888 wiring
  with no change of its own.
- **R12 retires.** The team-level control is the model enable-set; a
  provider restriction is unchecking that path's models, so the shipped
  `allowed_providers` control retires with the enable-set work rather than
  standing beside it as a coarser answer that can disagree. The one
  semantic that does not carry — a provider restriction auto-covered that
  provider's *future* models — is answered by the enable-set's own
  semantics: an untouched set tracks the default, a stored set is frozen
  at what it names.

Two hazards, recorded so the implementing tickets carry them:

- `addBedrockConfig` pins `ANTHROPIC_MODEL` to the org's stored
  `bedrock_model_id` for any model the native catalog cannot place, so an
  alias sent down a BYOK-Bedrock local path would be silently overridden.
  Lands with the local credential wiring (TFAC-888), which is also what
  makes a local BYOK path real at all. A second consequence for that
  ticket: with aliases the model no longer names the provider, so a local
  org holding both credentials needs an explicit active-path choice for
  the SDK environment — the existing provider picker becomes that choice,
  rather than merely selecting which credential to bind.
- The alias list (`fable` included) is asserted, not yet probed. The
  first implementation step is a sweep of the aliases through the SDK
  transport — engineering verification the probe machinery makes a button
  press, not a shipped surface.

Adding an SDK (Codex CLI, Gemini CLI, …) is an ordinary ticket, not an
epic: a new list, a credential shape, a probe transport, and the
harness-family work the 2026-08-21 amendment already buckets (prompt
text, tool surface, loop mechanics).

### 7.1 Budget caps: enforcement follows settlement

A budget cap acts wherever a **settled** number exists, and never on an
estimate. Settlement cadence is a property of the **runtime**, not the
mode:

- **Native runtime** settles per provider call — every message row carries
  real usage — so in-flight enforcement (the Q3 park) is available mid-run.
- **SDK runtime** settles at run completion (the full ReAct cycle, user
  message → final assistant message). Enforcement points are **dispatch
  boundaries**: a breach discovered at settle time gates the *next*
  dispatch and never kills work in flight. Overshoot is bounded by what
  was already running when the cap was crossed.
- In-flight SDK estimates are display-only (R18's `~`), enforcing nothing.

This is runtime-shaped on purpose: any multi SDK-driven surface inherits the
correct behavior — enforce at turn boundaries — with no special case,
matching the `conversations.runtime` vocabulary the
schema already speaks. And Q3.3's mid-flight blueprint halt works
identically in both runtimes, because a step is a run and step boundaries
are settle points in both.

**Subscription runs:** the settled figure is API-equivalent, not money
spent. TF never ships a budget gate on it, but a local user may configure
one knowingly ("stop me before my weekly limit") — never default, and the
cap UI states the basis is notional.
