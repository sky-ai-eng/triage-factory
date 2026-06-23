import { useState, useCallback, type ReactNode } from 'react'
import { AnimatePresence } from 'motion/react'
import PromptDrawer from './PromptDrawer'
import BindingGraph from './BindingGraph'
import ForgivingBanner from './ForgivingBanner'
import TaskRuleEditor from './TaskRuleEditor'
import TriggerConfigPanel from './TriggerConfigPanel'
import type { TriggerHandler, RuleHandler } from '../types'

// PromptsWorkspace is the single-team prompt/auto-delegation editor: the
// BindingGraph canvas plus the drawers that create/edit prompts, triggers, and
// task rules, and the forgiving banner that warns when an event type loses all
// coverage. Extracted from the standalone /prompts page (TFAC-445) so it can be
// dropped into the /team page's Prompts tab unchanged — both surfaces are the
// same editor over one team's rows; only the team-selection chrome differs (the
// standalone page owns a TeamSwitch; the /team tab inherits the page-level
// switcher). It fills its parent (h-full), so callers size the container.
//
// teamId is the active team — '' means solo/local (the server resolves the sole
// team). ready gates the BindingGraph's connect gesture + reads on a concrete
// scope (a multi-team user must not post team_id:'' in the cold-load window).
interface PromptsWorkspaceProps {
  teamId: string
  ready: boolean
  // Optional chrome rendered in the workspace's top bar, left of the New Prompt
  // button — the standalone page threads its title + TeamSwitch here; the /team
  // tab leaves them out (the page header owns both).
  toolbarLeft?: ReactNode
  toolbarRight?: ReactNode
}

export default function PromptsWorkspace({
  teamId,
  ready,
  toolbarLeft,
  toolbarRight,
}: PromptsWorkspaceProps) {
  const [selectedId, setSelectedId] = useState<string | null>(null)
  const [isNew, setIsNew] = useState(false)
  const [graphKey, setGraphKey] = useState(0)

  // Trigger config panel state.
  const [editingTrigger, setEditingTrigger] = useState<TriggerHandler | null>(null)

  // Forgiving banner state.
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
      const teamQuery = teamId ? `&team_id=${encodeURIComponent(teamId)}` : ''
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
    [teamId],
  )

  return (
    <div className="flex h-full flex-col">
      {/* Compact toolbar */}
      <div className="mb-4 flex shrink-0 items-center justify-between gap-2">
        <div className="flex min-w-0 items-center gap-2">{toolbarLeft}</div>
        <div className="flex items-center gap-2">
          {toolbarRight}
          <button
            onClick={openNew}
            className="rounded-full bg-accent px-4 py-2 text-[13px] font-semibold text-white transition-colors hover:bg-accent/90"
          >
            New Prompt
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
      <div className="min-h-0 flex-1">
        <BindingGraph
          key={graphKey}
          scope={{ kind: 'team', teamId }}
          scopeReady={ready}
          onPromptClick={openEdit}
          onTriggerClick={setEditingTrigger}
          onTriggerDeleted={handleTriggerDeleted}
        />
      </div>

      <PromptDrawer
        promptId={selectedId}
        isNew={isNew}
        lockedTeamId={teamId}
        onClose={closeDrawer}
        onSaved={handleSaved}
        onDeleted={handleDeleted}
      />

      {/* Task rule editor — opened by forgiving banner's "Create task rule" action */}
      <TaskRuleEditor
        open={ruleEditorOpen}
        rule={null}
        prefillEventType={bannerEventType ?? undefined}
        lockedTeamId={teamId}
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
