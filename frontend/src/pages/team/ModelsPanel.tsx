import { useMemo } from 'react'
import Table from '../../ui/table/Table'
import type { TableColumn, TableRow } from '../../ui/table/Table'
import type { ModelCatalogEntry } from '../../lib/models'
import { providerLabel } from '../../lib/models'
import type { TeamModelConfig, TeamModelPatch } from '../../hooks/useTeamModelConfig'
import type { TeamUsage } from '../../hooks/useTeamUsage'
import { effectiveModelKeys, money, spendShares } from './models'
import { httpErrorMessage } from '../../lib/apiClient'
import { toast } from '../../components/Toast/toastStore'

// CONFIGURED MODELS, opened: which of the organization's models this team may
// run, and which one an unset step falls back to. Everything else about
// inference — providers, credentials, which models exist at all — is the
// organization's, and lives on its own page.
//
// Only ORG-ENABLED models appear at all. A team admin never sees a model the
// organization has not enabled: it is not offerable, so listing it would be an
// inventory of other people's decisions. The recessed treatment belongs to rows
// this TEAM has not enabled, which is a state the reader can actually change —
// switching models on is the whole point of the surface — so an off row recedes
// by one ink step, not to the faintest ink in the ramp.
//
// The share column is % OF SPEND, deliberately not the mock's share of runs: no
// per-model run count exists, and the usage node's by_model cut is money. A
// column labeled for what it shows is the only honest version of the bar.

/** The price ink one step below a reading — cache rates, the wire id. */
const DIM_INK = 'color-mix(in srgb, var(--color-ink-3) 41%, var(--color-ink-2))'

export default function ModelsPanel({
  models,
  config,
  usage,
  isAdmin,
  onBack,
  onSave,
}: {
  /** The org's enable-set, in display order — what the catalog read answers. */
  models: ModelCatalogEntry[]
  config: TeamModelConfig | null
  usage: TeamUsage | null
  isAdmin: boolean
  onBack: () => void
  onSave: (patch: TeamModelPatch) => Promise<void>
}) {
  const enabledKeys = useMemo(() => effectiveModelKeys(models, config), [models, config])
  const shares = useMemo(() => spendShares(usage), [usage])

  const rows: TableRow[] = useMemo(() => {
    if (!config) return []
    const on = new Set(enabledKeys)
    return models.map((m, i) => ({
      id: m.key,
      ord: i,
      def: m.key === config.defaultModel,
      on: on.has(m.key),
      name: m.display_name,
      wire: m.key,
      provider: providerLabel(m) || '—',
      in: m.prices_per_mtok?.input ?? null,
      cw: m.prices_per_mtok?.cache_write ?? null,
      cr: m.prices_per_mtok?.cache_read ?? null,
      out: m.prices_per_mtok?.output ?? null,
      pct: on.has(m.key) ? Math.round((shares?.get(m.key) ?? 0) * 100) : 0,
    }))
  }, [models, config, enabledKeys, shares])

  const columns: TableColumn[] = useMemo(() => {
    const fail = (e: unknown) =>
      toast.error(httpErrorMessage(e, 'Could not save the model change.'))
    // The star is the same single-select mark the Jira status columns use for a
    // primary. It cannot be cleared, only moved: an unset step has to fall back
    // to something, so exactly one model always carries it. Every row offers
    // the slot, enabled or not — starring a model the team has not enabled
    // enables it, because making something the default is the strongest
    // possible statement that it should be available. One PATCH carries both
    // fields: the server validates the final state and does no implicit enable.
    const pick = (r: TableRow) => {
      if (r.def) return
      const grown = r.on ? null : enabledKeys.concat(String(r.id))
      void onSave({ ai_model: String(r.id), ...(grown ? { enabled_models: grown } : {}) }).catch(
        fail,
      )
    }
    // The set replaces wholesale on the wire, so a flip sends the whole
    // effective set with one membership changed — materializing "inherit
    // everything" into an explicit list the first time a team narrows it.
    const flip = (r: TableRow) => {
      if (r.def) return
      const next = r.on ? enabledKeys.filter((k) => k !== r.id) : enabledKeys.concat(String(r.id))
      void onSave({ enabled_models: next }).catch(fail)
    }
    const writes: TableColumn[] = isAdmin
      ? [
          { key: 'def', label: '', type: 'mark', isSet: (r: TableRow) => r.def, onPick: pick },
          {
            key: 'on',
            label: 'ENABLED',
            width: '62px',
            type: 'toggle',
            onToggle: flip,
            // The default's own toggle is inert: the fallback can be handed to
            // another model, never switched off.
            locked: (r: TableRow) => r.def,
          },
        ]
      : []
    return [
      ...writes,
      {
        key: 'name',
        label: 'MODEL',
        width: '268px',
        mono: false,
        color: (r) => (r.on ? 'var(--color-ink-1)' : 'var(--color-ink-2)'),
      },
      // The id is the only thing that says what this row will put on the wire —
      // the same display name is spelled many ways across providers, and the id
      // carries the access path for free.
      { key: 'wire', label: 'ID ON THE WIRE', color: () => DIM_INK },
      { key: 'provider', label: 'PROVIDER', width: '104px', color: () => 'var(--color-ink-3)' },
      {
        key: 'in',
        label: 'IN',
        align: 'end',
        render: (r) => money(r.in),
        color: () => 'var(--color-ink-2)',
      },
      // The cache rates come from the API's own price fields, never recomputed
      // from multipliers here — the datasheet is the authority on what a warm
      // conversation costs.
      {
        key: 'cw',
        label: 'CACHE WRITE',
        align: 'end',
        render: (r) => money(r.cw),
        color: () => DIM_INK,
      },
      {
        key: 'cr',
        label: 'CACHE READ',
        align: 'end',
        render: (r) => money(r.cr),
        color: () => DIM_INK,
      },
      {
        key: 'out',
        label: 'OUT',
        align: 'end',
        render: (r) => money(r.out),
        color: () => 'var(--color-ink-2)',
      },
      {
        key: 'pct',
        label: '% OF SPEND',
        type: 'bar',
        width: '112px',
        // Three answers, three inks. A team-unenabled row has nothing to
        // report — dash. An enabled row with no spend reports 0%, a reading
        // and the argument for turning it off. And every row is a dash until
        // the usage read answers, which for a member is never: the read is
        // admin-gated and unknown is not zero.
        format: (v: number, r: TableRow) => (shares && r.on ? v + '%' : '—'),
        color: (r) => (r.pct ? 'var(--color-warm)' : 'transparent'),
        valueColor: (r: TableRow) => (shares && r.on ? 'var(--color-ink-2)' : 'var(--color-ink-4)'),
      },
    ]
  }, [isAdmin, enabledKeys, shares, onSave])

  return (
    <div className="ts-panelview">
      <div className="ts-tablewrap">
        <Table
          build
          columns={columns}
          rows={rows}
          pageSize="auto"
          sortKey="ord"
          selectable={false}
          emptyLabel="—"
          // The way back sits where the table's label would, so the panel is an
          // ordinary table rather than a table under a header of its own.
          headerLeft={
            <button type="button" className="ts-back" onClick={onBack}>
              <i className="ts-back-chev" aria-hidden="true" />
              <span>CONFIGURED MODELS</span>
            </button>
          }
          rowBg={(r) =>
            r.def ? 'color-mix(in srgb, var(--color-warm) 8%, var(--color-ground))' : null
          }
          footer={<span className="ts-models-foot">dollars per million tokens</span>}
        />
      </div>
    </div>
  )
}
