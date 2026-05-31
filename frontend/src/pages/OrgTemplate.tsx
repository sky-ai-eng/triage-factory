import { useState, useCallback } from 'react'
import { Navigate } from 'react-router-dom'
import { AnimatePresence } from 'motion/react'
import { Layers } from 'lucide-react'
import PromptDrawer from '../components/PromptDrawer'
import BindingGraph from '../components/BindingGraph'
import ForgivingBanner from '../components/ForgivingBanner'
import TaskRuleEditor from '../components/TaskRuleEditor'
import TriggerConfigPanel from '../components/TriggerConfigPanel'
import { useTemplateScope } from '../hooks/useTemplateScope'
import { useOrgHref } from '../hooks/useOrgHref'
import { handlersBase } from '../lib/scope'
import type { TriggerHandler, RuleHandler } from '../types'

// OrgTemplate is the org-admin editor over the template new teams are seeded
// from (SKY-381). It is the SAME binding-graph editor as /prompts, at template
// scope — but deliberately, unmistakably NOT a team: no TeamSwitch, a distinct
// header + a persistent forward-only banner + an accent frame on the canvas.
export default function OrgTemplate() {
  const orgHref = useOrgHref()
  const { available, ready, loading } = useTemplateScope()

  const [selectedId, setSelectedId] = useState<string | null>(null)
  const [isNew, setIsNew] = useState(false)
  const [graphKey, setGraphKey] = useState(0)
  const [editingTrigger, setEditingTrigger] = useState<TriggerHandler | null>(null)
  const [bannerEventType, setBannerEventType] = useState<string | null>(null)
  const [ruleEditorOpen, setRuleEditorOpen] = useState(false)

  const openNew = () => {
    setSelectedId(null)
    setIsNew(true)
  }
  const openEdit = useCallback((id: string) => {
    setIsNew(false)
    setSelectedId(id)
  }, [])
  const closeDrawer = () => {
    setSelectedId(null)
    setIsNew(false)
  }
  const handleSaved = () => {
    closeDrawer()
    setGraphKey((k) => k + 1)
  }
  const handleDeleted = () => {
    closeDrawer()
    setGraphKey((k) => k + 1)
  }

  // Coverage check after a trigger delete — scoped to the org template.
  const handleTriggerDeleted = useCallback(async (eventType: string) => {
    const base = handlersBase(true)
    try {
      const [triggersRes, rulesRes] = await Promise.all([
        fetch(`${base}?kind=trigger`).then((r) => {
          if (!r.ok) throw new Error()
          return r.json()
        }),
        fetch(`${base}?kind=rule`).then((r) => {
          if (!r.ok) throw new Error()
          return r.json()
        }),
      ])
      const hasTrigger = (triggersRes as TriggerHandler[]).some(
        (t) => t.event_type === eventType && t.enabled,
      )
      const hasRule = (rulesRes as RuleHandler[]).some(
        (r) => r.event_type === eventType && r.enabled,
      )
      if (!hasTrigger && !hasRule) {
        setBannerEventType(eventType)
      }
    } catch {
      // Network error — don't show banner, not a coverage gap.
    }
  }, [])

  // Non-admins (and local mode) can't reach the template — redirect to the
  // org root. Gate server-side too (requireOrgTemplate); this is the UI half.
  // Only redirect once auth has confirmed the viewer isn't an admin — while
  // loading, hold so an admin isn't bounced during the cold load. (AuthGate
  // already blocks rendering until authed, so this is belt-and-suspenders.)
  if (!loading && !available) {
    return <Navigate to={orgHref('/')} replace />
  }

  return (
    <div className="flex flex-col" style={{ height: 'calc(100vh - 120px)' }}>
      {/* Distinct header — never reads as a live team's /prompts. */}
      <div className="flex items-center justify-between mb-3 shrink-0">
        <div className="flex items-center gap-2.5">
          <span className="flex h-7 w-7 items-center justify-center rounded-lg bg-accent-soft text-accent">
            <Layers size={15} />
          </span>
          <div>
            <h1 className="text-[17px] font-semibold text-text-primary leading-tight">
              Org Template — Defaults for New Teams
            </h1>
            <p className="text-[11px] text-text-tertiary leading-tight">
              The prompts, rules, and triggers every new team starts with.
            </p>
          </div>
        </div>
        <button
          onClick={openNew}
          className="text-[13px] font-semibold text-white bg-accent hover:bg-accent/90 px-4 py-2 rounded-full transition-colors"
        >
          New Prompt
        </button>
      </div>

      {/* Persistent forward-only banner (load-bearing per SKY-381). */}
      <div className="mb-3 shrink-0 rounded-xl border border-amber-300/60 bg-amber-50/70 px-4 py-2.5">
        <p className="text-[12px] text-amber-900">
          <span className="font-semibold">Changes apply to teams created from now on.</span>{' '}
          Existing teams are untouched — editing the template only changes what the next new team
          inherits.
        </p>
      </div>

      {/* Forgiving banner — appears when an event type loses all coverage. */}
      <AnimatePresence>
        {bannerEventType && (
          <ForgivingBanner
            eventType={bannerEventType}
            onCreateRule={() => setRuleEditorOpen(true)}
            onDismiss={() => setBannerEventType(null)}
          />
        )}
      </AnimatePresence>

      {/* Accent frame so the canvas is visually distinct from a team's graph. */}
      <div className="flex-1 min-h-0 rounded-2xl ring-2 ring-accent/30 ring-offset-2 ring-offset-surface overflow-hidden">
        <BindingGraph
          key={graphKey}
          scope={{ kind: 'template' }}
          scopeReady={ready}
          onPromptClick={openEdit}
          onTriggerClick={setEditingTrigger}
          onTriggerDeleted={handleTriggerDeleted}
        />
      </div>

      <PromptDrawer
        promptId={selectedId}
        isNew={isNew}
        templateScope
        onClose={closeDrawer}
        onSaved={handleSaved}
        onDeleted={handleDeleted}
      />

      <TaskRuleEditor
        open={ruleEditorOpen}
        rule={null}
        prefillEventType={bannerEventType ?? undefined}
        templateScope
        onClose={() => setRuleEditorOpen(false)}
        onSaved={() => {
          setRuleEditorOpen(false)
          setBannerEventType(null)
          setGraphKey((k) => k + 1)
        }}
      />

      <TriggerConfigPanel
        open={editingTrigger !== null}
        trigger={editingTrigger}
        templateScope
        onClose={() => setEditingTrigger(null)}
        onSaved={() => {
          setEditingTrigger(null)
          setGraphKey((k) => k + 1)
        }}
        onDeleted={() => {
          const eventType = editingTrigger?.event_type
          setEditingTrigger(null)
          setGraphKey((k) => k + 1)
          if (eventType) handleTriggerDeleted(eventType)
        }}
        onRefresh={() => setGraphKey((k) => k + 1)}
      />
    </div>
  )
}
