import { useState, useCallback } from 'react'
import { AnimatePresence } from 'motion/react'
import BlueprintDrawer from '../components/BlueprintDrawer'
import BindingGraph from '../components/BindingGraph'
import ForgivingBanner from '../components/ForgivingBanner'
import TaskRuleEditor from '../components/TaskRuleEditor'
import TriggerConfigPanel from '../components/TriggerConfigPanel'
import TeamSwitch from '../components/TeamSwitch'
import { useActiveTeam } from '../hooks/useTeams'
import type { TriggerHandler, RuleHandler } from '../types'

export default function Prompts() {
  // The graph nodes are blueprints, so the editor drawer carries a blueprint id.
  const [selectedBlueprintId, setSelectedBlueprintId] = useState<string | null>(null)
  const [isNew, setIsNew] = useState(false)
  const [graphKey, setGraphKey] = useState(0)

  // The prompts page is single-team: it doubles as the auto-delegation
  // binding graph, so it shows exactly one team's prompts + triggers, and
  // new prompts / triggers / rules created here belong to that team. The
  // switcher renders only for ≥2-team users; solo/local keeps teamId ''
  // (server resolves the sole team) and the page is unchanged.
  const activeTeam = useActiveTeam('prompts')

  // Trigger config panel state.
  const [editingTrigger, setEditingTrigger] = useState<TriggerHandler | null>(null)

  // Forgiving banner state.
  const [bannerEventType, setBannerEventType] = useState<string | null>(null)
  const [ruleEditorOpen, setRuleEditorOpen] = useState(false)

  const openNew = () => {
    setSelectedBlueprintId(null)
    setIsNew(true)
  }

  const openEdit = useCallback((blueprintId: string) => {
    setIsNew(false)
    setSelectedBlueprintId(blueprintId)
  }, [])

  const closeDrawer = () => {
    setSelectedBlueprintId(null)
    setIsNew(false)
  }

  const handleSaved = () => {
    closeDrawer()
    setGraphKey((k) => k + 1) // force graph refetch
  }

  const handleDeleted = () => {
    closeDrawer()
    setGraphKey((k) => k + 1)
  }

  // After a trigger is deleted, check if the event_type is still covered by
  // any remaining triggers or enabled task_rules — scoped to the active
  // team, since coverage is per-team. If not, show the banner.
  const handleTriggerDeleted = useCallback(
    async (eventType: string) => {
      const teamQuery = activeTeam.teamId ? `&team_id=${encodeURIComponent(activeTeam.teamId)}` : ''
      try {
        const [triggersRes, rulesRes] = await Promise.all([
          fetch(`/api/event-handlers?kind=trigger${teamQuery}`).then((r) => {
            if (!r.ok) throw new Error()
            return r.json()
          }),
          fetch(`/api/event-handlers?kind=rule${teamQuery}`).then((r) => {
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
    },
    [activeTeam.teamId],
  )

  return (
    <div className="flex flex-col" style={{ height: 'calc(100vh - 120px)' }}>
      {/* Compact header */}
      <div className="flex items-center justify-between mb-4 shrink-0">
        <h1 className="text-[17px] font-semibold text-text-primary">Prompts</h1>
        <div className="flex items-center gap-2">
          <TeamSwitch value={activeTeam.teamId} onChange={activeTeam.setTeamId} />
          <button
            onClick={openNew}
            className="text-[13px] font-semibold text-white bg-accent hover:bg-accent/90 px-4 py-2 rounded-full transition-colors"
          >
            New Blueprint
          </button>
        </div>
      </div>

      {/* Forgiving banner — appears above the graph when an event_type loses all coverage */}
      <AnimatePresence>
        {bannerEventType && (
          <ForgivingBanner
            eventType={bannerEventType}
            onCreateRule={() => setRuleEditorOpen(true)}
            onDismiss={() => setBannerEventType(null)}
          />
        )}
      </AnimatePresence>

      {/* Graph fills remaining space */}
      <div className="flex-1 min-h-0">
        <BindingGraph
          key={graphKey}
          scope={{ kind: 'team', teamId: activeTeam.teamId }}
          scopeReady={activeTeam.ready}
          onBlueprintClick={openEdit}
          onTriggerClick={setEditingTrigger}
          onTriggerDeleted={handleTriggerDeleted}
        />
      </div>

      <BlueprintDrawer
        blueprintId={selectedBlueprintId}
        isNew={isNew}
        lockedTeamId={activeTeam.teamId}
        onClose={closeDrawer}
        onSaved={handleSaved}
        onDeleted={handleDeleted}
      />

      {/* Task rule editor — opened by forgiving banner's "Create task rule" action */}
      <TaskRuleEditor
        open={ruleEditorOpen}
        rule={null}
        prefillEventType={bannerEventType ?? undefined}
        lockedTeamId={activeTeam.teamId}
        onClose={() => setRuleEditorOpen(false)}
        onSaved={() => {
          setRuleEditorOpen(false)
          setBannerEventType(null)
        }}
      />

      {/* Trigger config panel — opened by clicking a trigger edge */}
      <TriggerConfigPanel
        open={editingTrigger !== null}
        trigger={editingTrigger}
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
