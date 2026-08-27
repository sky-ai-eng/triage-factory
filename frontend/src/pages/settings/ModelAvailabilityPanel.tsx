// The org's model list: every model this deployment offers, what it costs, and
// whether this organization's credentials can actually run it — with a test
// button per row.
//
// It shows models the org has NOT enabled too. Availability is org truth about
// what the credentials can do, not about what teams may pick, and an admin
// deciding whether to enable a model wants to know it works first; the sweep
// endpoint takes the same view and narrows by no enable-set either.
//
// TODO(TFAC-906): this is a table, and it is hand-rolled flex rows because the
// org settings stack around it holds no design-system component. It becomes a
// ui/table/Table — a render column for the test button — when that surface is
// cut over; doing it before would make it the only instrument in a stack that
// is being retired.
//
// THE PANEL EXISTS EXACTLY WHEN THE ORG BRINGS ITS OWN CREDENTIALS, and it
// works that out from the rows rather than from a mode: a verdict is a fact
// about a credential TF owns, so a row with no availability field is one no
// probe could be about — and a test button beside it would be a control the
// route refuses.

import { useState } from 'react'
import { toast } from '../../components/Toast/toastStore'
import ModelAvailabilityBadge from '../../components/ModelAvailabilityBadge'
import { refreshModelCatalog, useModelCatalog } from '../../hooks/useModelCatalog'
import { httpErrorMessage } from '../../lib/apiClient'
import {
  availabilityHelp,
  lacksPromptCaching,
  probeCostSentence,
  secondaryLine,
  testModel,
  PROMPT_CACHING_WARNING,
  type ModelCatalogEntry,
} from '../../lib/models'

export default function ModelAvailabilityPanel({ orgId }: { orgId: string | null }) {
  const { all, loaded } = useModelCatalog()
  const [testing, setTesting] = useState('')

  if (!loaded) return null
  const rows = all.filter((m) => m.availability !== undefined)
  if (rows.length === 0) {
    return (
      <p className="text-[13px] text-ink-3">
        This workspace runs on the Claude credentials of the machine hosting it, so there is nothing
        stored to test against. Connect a provider above to test models against your own
        credentials.
      </p>
    )
  }

  const test = async (m: ModelCatalogEntry) => {
    if (!orgId) return
    setTesting(m.key)
    try {
      const result = await testModel(orgId, m.key)
      // The badge is a stored verdict written server-side, so the row is
      // re-read rather than patched from the response — the two would be the
      // same fact in two places, and only one of them is the one a reload shows.
      await refreshModelCatalog(orgId)
      switch (result.outcome) {
        case 'verified':
          toast.success(`${m.display_name} is available.`)
          break
        case 'red':
          toast.error(`${m.display_name}: ${result.detail || 'your provider refused this model.'}`)
          break
        default:
          // Nothing was recorded, so the badge still says what it said. Saying
          // "we could not tell" is the honest report; a red badge here would
          // turn one bad provider minute into a model that looks broken.
          toast.warning(
            `${m.display_name}: ${result.detail || 'nobody answered. Nothing was recorded — try again.'}`,
          )
      }
    } catch (e) {
      toast.error(httpErrorMessage(e, 'The test could not be run.'))
    } finally {
      setTesting('')
    }
  }

  return (
    <div className="divide-y divide-[var(--color-line-1)] border-y border-line-1">
      {rows.map((m) => {
        const help = availabilityHelp(m)
        const secondary = secondaryLine(m)
        return (
          <div key={m.key} className="flex items-start justify-between gap-3 py-2.5">
            <div className="min-w-0 space-y-1">
              <span className="flex items-center gap-2">
                <span className="text-body font-medium text-ink-1">{m.display_name}</span>
                <ModelAvailabilityBadge state={m.availability} help={help} />
                {!m.enabled && (
                  <span className="shrink-0 text-reported text-ink-3">not enabled</span>
                )}
              </span>
              {secondary && <span className="block text-reported text-ink-3">{secondary}</span>}
              {lacksPromptCaching(m) && (
                <span className="block text-reported text-warm">{PROMPT_CACHING_WARNING}</span>
              )}
              {help && <span className="block text-reported text-ink-3">{help}</span>}
            </div>
            <button
              type="button"
              onClick={() => void test(m)}
              disabled={testing !== '' || !orgId}
              // Pressing this IS the consent, so the button says what it spends
              // before it is pressed rather than after.
              title={probeCostSentence(m)}
              className="shrink-0 rounded-[5px] border border-line-1 px-2.5 py-1 text-ui font-medium text-ink-2 transition-colors hover:bg-tint-3 hover:text-ink-1 disabled:opacity-50"
            >
              {testing === m.key ? 'Testing…' : 'Test connection'}
            </button>
          </div>
        )
      })}
    </div>
  )
}
