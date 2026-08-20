import { useState, useCallback, type ReactNode } from 'react'
import { AnimatePresence } from 'motion/react'
import PromptDrawer from './PromptDrawer'
import BindingGraph from './BindingGraph'
import ForgivingBanner from './ForgivingBanner'
import TaskRuleEditor from './TaskRuleEditor'
import TriggerConfigPanel from './TriggerConfigPanel'
import ViewOnlyBadge from './ViewOnlyBadge'
import { useTeamRole } from '../hooks/useTeamRole'
import type { TriggerHandler, RuleHandler } from '../types'
import { apiListAll } from '../lib/apiClient'

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

  // A viewer of this team gets a read-only graph: the create affordance is
  // withheld and a quiet "view-only" chip stands in (TFAC-447). Prompt/trigger
  // edits opened from the canvas still surface the backend's 403 if attempted —
  // the server RLS is the hard guarantee; this just stops offering writes a
  // viewer can't make. Solo/local reports "admin", so the editor stays fully
  // interactive there.
  //
  // rolesLoaded gates the toolbar's create/badge: until /api/teams resolves the
  // role is unknown, so rendering neither (rather than the permissive "New
  // Prompt" default) avoids flashing a write affordance at a viewer for the
  // cold-load window before it flips to the badge. The drawers read `readOnly`
  // directly — they only open on a canvas click, by which point roles are loaded.
  const { isViewer, loaded: rolesLoaded } = useTeamRole()
  const readOnly = isViewer(teamId)

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
      const teamFilter = teamId ? { team_id: teamId } : {}
      try {
        // Walked to completion on both: this asks whether ANY remaining handler
        // still covers the event type, and a partial read would answer "no
        // coverage" for a handler that simply sat on a later page.
        const [triggersRes, rulesRes] = await Promise.all([
          apiListAll<TriggerHandler>('/api/event-handlers/list', {
            kind: 'trigger',
            ...teamFilter,
          }),
          apiListAll<RuleHandler>('/api/event-handlers/list', { kind: 'rule', ...teamFilter }),
        ])
        const hasTrigger = triggersRes.some((t) => t.event_type === eventType && t.enabled)
        const hasRule = rulesRes.some((r) => r.event_type === eventType && r.enabled)
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
          {/* Until roles resolve, render neither — see rolesLoaded note above. */}
          {rolesLoaded &&
            (readOnly ? (
              <ViewOnlyBadge />
            ) : (
              <button
                onClick={openNew}
                className="rounded-full bg-warm px-4 py-2 text-body font-semibold text-warm-ink transition-colors hover:bg-warm/90"
              >
                New Prompt
              </button>
            ))}
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
          scope={{ teamId }}
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
        readOnly={readOnly}
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
        readOnly={readOnly}
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
