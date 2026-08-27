// The model surface's client half: the row shape `GET /api/orgs/{org}/models`
// answers with, the two test routes, and the pure readings of a row every
// picker and badge shares.
//
// ONE CONTRACT, TWO UNIVERSES, and the difference arrives as ABSENT FIELDS —
// never as a mode this module reads. A row that names a provider joins the
// pricing datasheet and carries prices, a context window and a caching fact; a
// row whose harness resolves the access path from the credential carries none
// of them, because the provider is a property of that credential and the cost
// is settled by the harness. Availability is its own presence gate: it is
// published exactly when the org brings its own credentials, so a row without it
// is not "unknown" — it is a row no stored verdict can be about. Every reading
// below is written as "what is present", and nothing here infers a mode.

import { apiJSON, apiFetch } from './apiClient'

/** Headline list prices in dollars per million tokens. Display only — what a
 *  run actually cost is recorded per message in the ledger. */
export interface ModelPricesPerMTok {
  input: number
  output: number
  cache_read: number
  cache_write: number
}

/** The four availability states the read publishes, and the absence of the
 *  field is the fifth answer — see the module doc. */
export type ModelAvailability = 'unconfigured' | 'unverified' | 'verified' | 'red'

/** One offered model, exactly as the models read returns it. Optional fields
 *  are optional ON THE WIRE: absent means TF asserts nothing, and a client
 *  renders what is present rather than inventing a zero, a provider, or a
 *  badge. */
export interface ModelCatalogEntry {
  key: string
  display_name: string
  provider?: string
  provider_display_name?: string
  enabled: boolean
  prices_per_mtok?: ModelPricesPerMTok
  context_window?: number
  supports_prompt_caching?: boolean
  availability?: ModelAvailability
  availability_detail?: string
  availability_checked_at?: string
}

// The read also carries `display_order`, and nothing here reads it: the items
// ARRIVE in that order and no client-side sort exists to need it. A picker
// renders the order it was given, which is the registry's, and that order
// asserts nothing about capability.

/** One model's test outcome. `verified` and `red` are also availability states,
 *  spelled the same way so the two can be compared directly; the other two name
 *  things a test can do that a stored state cannot — `inconclusive` wrote
 *  nothing at all, and `skipped` passed over a model already verified. */
export type ModelTestOutcome = 'verified' | 'red' | 'inconclusive' | 'skipped'

export interface ModelTestResult {
  model_key: string
  outcome: ModelTestOutcome
  detail?: string
  checked_at?: string
}

export interface ModelTestSweep {
  provider: string
  items: ModelTestResult[]
}

/** testModel spends one paid request against the org's credentials and records
 *  what it establishes. A 200 is the TEST having run, not the model being
 *  available: a refusal is a successful test with a negative answer. */
export async function testModel(orgId: string, modelKey: string): Promise<ModelTestResult> {
  return apiJSON<ModelTestResult>(
    `/api/orgs/${encodeURIComponent(orgId)}/models/${encodeURIComponent(modelKey)}/test`,
    { method: 'POST' },
  )
}

/** sweepModelTests tests every not-yet-verified candidate of one provider — the
 *  eager pass an admin consents to after connecting that provider's credential.
 *  Already-verified rows come back `skipped` and cost nothing. */
export async function sweepModelTests(orgId: string, provider: string): Promise<ModelTestSweep> {
  const resp = await apiFetch(`/api/orgs/${encodeURIComponent(orgId)}/models/tests`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ provider }),
  })
  return (await resp.json()) as ModelTestSweep
}

/** providerLabel is how a row's access path reads, or '' where the id names
 *  none. The label travels on the row itself, so this never keeps a second copy
 *  of the provider vocabulary — a map here could only fall behind the build's. */
export function providerLabel(m: ModelCatalogEntry): string {
  return m.provider_display_name ?? m.provider ?? ''
}

/** outputPriceLabel renders the headline output rate, or '' for a row whose
 *  cost the harness settles. Output alone, because it is the rate that
 *  dominates an agent run's bill and the one a person compares models on;
 *  the full four are on the row for anything that wants them. */
export function outputPriceLabel(m: ModelCatalogEntry): string {
  const prices = m.prices_per_mtok
  if (!prices) return ''
  return `$${prices.output.toFixed(2)}/Mtok out`
}

/** secondaryLine is the picker's under-label text: the access path and the
 *  headline rate, whichever of them this row actually carries. */
export function secondaryLine(m: ModelCatalogEntry): string {
  return [providerLabel(m), outputPriceLabel(m)].filter(Boolean).join(' · ')
}

/** lacksPromptCaching reports a model that TF knows cannot cache its prompt.
 *  Strictly `false`, never absent: a row that makes no claim is not a row
 *  claiming "no". */
export function lacksPromptCaching(m: ModelCatalogEntry): boolean {
  return m.supports_prompt_caching === false
}

/** PROMPT_CACHING_WARNING is what a person needs to know about picking one:
 *  every run re-sends the whole prompt at full rate. */
export const PROMPT_CACHING_WARNING =
  'Runs cost several times more without prompt caching — the whole prompt is re-sent on every turn.'

/** selectable reports whether a row may be chosen. A model the org holds no
 *  credential for, or one a probe was refused for, is listed and not
 *  selectable: hiding it would leave a person hunting for a model they were
 *  shown yesterday, and the save would refuse it anyway. */
export function selectable(m: ModelCatalogEntry): boolean {
  return m.availability !== 'red' && m.availability !== 'unconfigured'
}

/** needsTestBeforeSave reports whether saving this selection has to establish
 *  its availability first. Only `unverified` does: `verified` is permanent, and
 *  a row with NO availability field never gates — nothing is stored about it,
 *  so there is nothing a test could conclude. */
export function needsTestBeforeSave(m: ModelCatalogEntry): boolean {
  return m.availability === 'unverified'
}

/** probeCostSentence states what one test of this model spends, in the terms of
 *  the transport that will run it. The transport is not a mode the client
 *  reads: a row carrying prices is dispatched over the direct API and a probe
 *  there is a two-token request, while a row whose cost the harness settles is
 *  probed through that harness and spends a whole invocation. */
export function probeCostSentence(m: ModelCatalogEntry): string {
  return m.prices_per_mtok
    ? 'This spends one real request against your credentials — about two tokens.'
    : 'This spends one real run of the agent harness against your credentials, which costs more than a bare API call.'
}

/** availabilityLabel is the badge's word. */
export function availabilityLabel(state: ModelAvailability): string {
  switch (state) {
    case 'verified':
      return 'Verified'
    case 'red':
      return 'Unavailable'
    case 'unconfigured':
      return 'Not connected'
    case 'unverified':
      return 'Untested'
  }
}

/** availabilityHelp NAMES THE FIX rather than restating the state. A badge that
 *  only says "unavailable" leaves an admin with nowhere to go; the provider's
 *  own refusal is the actionable half, so it is what a red badge carries. */
export function availabilityHelp(m: ModelCatalogEntry): string {
  switch (m.availability) {
    case 'red':
      return m.availability_detail
        ? `${m.availability_detail} — fix it with your provider, then test again.`
        : 'Your provider refused this model. Fix it with your provider, then test again.'
    case 'unconfigured':
      return 'Connect this model’s provider in Settings → Claude credentials to use it.'
    case 'unverified':
      return 'Nothing has established that your credentials can run this model yet. Testing it settles that.'
    case 'verified':
      return 'Your credentials ran this model successfully.'
    default:
      return ''
  }
}
