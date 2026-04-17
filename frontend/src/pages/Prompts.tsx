import { useState, useCallback } from 'react'
import { AnimatePresence } from 'motion/react'
import PromptDrawer from '../components/PromptDrawer'
import BindingGraph from '../components/BindingGraph'
import ForgivingBanner from '../components/ForgivingBanner'
import TaskRuleEditor from '../components/TaskRuleEditor'
import type { PromptTrigger, TaskRule } from '../types'

export default function Prompts() {
  const [selectedId, setSelectedId] = useState<string | null>(null)
  const [isNew, setIsNew] = useState(false)
  const [graphKey, setGraphKey] = useState(0)

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
  // any remaining triggers or enabled task_rules. If not, show the banner.
  const handleTriggerDeleted = useCallback(async (eventType: string) => {
    try {
      const [triggersRes, rulesRes] = await Promise.all([
        fetch('/api/triggers').then((r) => r.json()),
        fetch('/api/task-rules').then((r) => r.json()),
      ])
      const hasTrigger = (triggersRes as PromptTrigger[]).some(
        (t) => t.event_type === eventType && t.enabled,
      )
      const hasRule = (rulesRes as TaskRule[]).some((r) => r.event_type === eventType && r.enabled)
      if (!hasTrigger && !hasRule) {
        setBannerEventType(eventType)
      }
    } catch {
      // Network error — don't show banner, not a coverage gap.
    }
  }, [])

  return (
    <div className="flex flex-col" style={{ height: 'calc(100vh - 120px)' }}>
      {/* Compact header */}
      <div className="flex items-center justify-between mb-4 shrink-0">
        <h1 className="text-[17px] font-semibold text-text-primary">Prompts</h1>
        <button
          onClick={openNew}
          className="text-[13px] font-semibold text-white bg-accent hover:bg-accent/90 px-4 py-2 rounded-full transition-colors"
        >
          New Prompt
        </button>
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
          onPromptClick={openEdit}
          onTriggerDeleted={handleTriggerDeleted}
        />
      </div>

      <PromptDrawer
        promptId={selectedId}
        isNew={isNew}
        onClose={closeDrawer}
        onSaved={handleSaved}
        onDeleted={handleDeleted}
      />

      {/* Task rule editor — opened by forgiving banner's "Create task rule" action */}
      <TaskRuleEditor
        open={ruleEditorOpen}
        rule={null}
        prefillEventType={bannerEventType ?? undefined}
        onClose={() => setRuleEditorOpen(false)}
        onSaved={() => {
          setRuleEditorOpen(false)
          setBannerEventType(null)
        }}
      />
    </div>
  )
}
