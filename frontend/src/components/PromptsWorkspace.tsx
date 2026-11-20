import { useState, useCallback, useEffect, useRef, type ReactNode } from 'react'
import { AnimatePresence } from 'motion/react'
import PromptDrawer from './PromptDrawer'
import BindingGraph from './BindingGraph'
import ForgivingBanner from './ForgivingBanner'
import TaskRuleEditor from './TaskRuleEditor'
import TriggerConfigPanel from './TriggerConfigPanel'
import ViewOnlyBadge from './ViewOnlyBadge'
import { useTeamRole } from '../hooks/useTeamRole'
import { toast } from './Toast/toastStore'
import type { EventHandler } from '../types'
import { countEventHandlers, getEventHandler, hasBlueprint } from '../lib/eventHandlers'

// PromptsWorkspace is the single-team prompt/auto-delegation editor: the
// BindingGraph canvas plus the drawers that create/edit prompts and event
// handlers, and the forgiving banner that warns when an event type loses all
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
  // openTriggerId opens that event handler's config panel, as a link to it
  // asks. onTriggerOpened is called once the handler has been read, with its
  // team, or null when there is no handler to open, so the caller can switch
  // to the team and drop the link.
  openTriggerId?: string | null
  onTriggerOpened?: (triggerTeam: string | null) => void
}

export default function PromptsWorkspace({
  teamId,
  ready,
  toolbarLeft,
  toolbarRight,
  openTriggerId,
  onTriggerOpened,
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
  const { isViewer, roleForTeam, loaded: rolesLoaded } = useTeamRole()
  const readOnly = isViewer(teamId)

  // Handler config panel state. foreignTrigger is a linked handler of a team
  // the viewer is not on: the page cannot switch to that team, so the panel
  // opens it read-only rather than by the active team's role.
  const [editingTrigger, setEditingTrigger] = useState<EventHandler | null>(null)
  const [foreignTrigger, setForeignTrigger] = useState(false)
  const openTrigger = useCallback((t: EventHandler | null, foreign = false) => {
    setEditingTrigger(t)
    setForeignTrigger(foreign)
  }, [])

  // The read runs once per linked id: the callbacks it ends with are read
  // through refs, so a new identity for either neither refetches nor reports
  // the handler twice.
  const linkRef = useRef({ onTriggerOpened, roleForTeam })
  useEffect(() => {
    linkRef.current = { onTriggerOpened, roleForTeam }
  })
  useEffect(() => {
    if (!openTriggerId) return
    let cancelled = false
    getEventHandler(openTriggerId)
      .then((h) => {
        if (cancelled) return
        const { onTriggerOpened, roleForTeam } = linkRef.current
        // The graph draws a handler only by its blueprint, so one with none
        // has nowhere to open here.
        if (!hasBlueprint(h)) {
          toast.error('That event handler has no blueprint to show on the canvas.')
          onTriggerOpened?.(null)
          return
        }
        openTrigger(h, roleForTeam(h.team_id) === null)
        onTriggerOpened?.(h.team_id)
      })
      .catch((err) => {
        if (cancelled) return
        toast.failed(err, 'Could not open the linked event handler.')
        linkRef.current.onTriggerOpened?.(null)
      })
    return () => {
      cancelled = true
    }
  }, [openTriggerId, openTrigger])

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

  // After a handler is deleted or detached, check whether the event_type
  // still makes a task for the active team — coverage is per-team. If not,
  // show the banner. One count-only read asks the question whole.
  const handleTriggerDeleted = useCallback(
    async (eventType: string) => {
      try {
        const covering = await countEventHandlers({
          ...(teamId ? { team_id: teamId } : {}),
          event_type: eventType,
          modes: ['watch', 'automate'],
        })
        if (covering === 0) {
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
          onTriggerClick={openTrigger}
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

      {/* Event handler editor — opened by the forgiving banner's create action */}
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

      {/* Handler config panel — opened by clicking a handler edge */}
      <TriggerConfigPanel
        open={editingTrigger !== null}
        trigger={editingTrigger}
        readOnly={readOnly || foreignTrigger}
        onClose={() => openTrigger(null)}
        onSaved={() => {
          openTrigger(null)
          setGraphKey((k) => k + 1)
        }}
        onDeleted={() => {
          const eventType = editingTrigger?.event_type
          openTrigger(null)
          setGraphKey((k) => k + 1)
          if (eventType) handleTriggerDeleted(eventType)
        }}
        onRefresh={() => setGraphKey((k) => k + 1)}
      />
    </div>
  )
}
