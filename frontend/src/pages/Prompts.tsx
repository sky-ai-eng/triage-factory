import { useState, useCallback } from 'react'
import PromptDrawer from '../components/PromptDrawer'
import BindingGraph from '../components/BindingGraph'

export default function Prompts() {
  const [selectedId, setSelectedId] = useState<string | null>(null)
  const [isNew, setIsNew] = useState(false)
  const [graphKey, setGraphKey] = useState(0)

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
    setGraphKey(k => k + 1) // force graph refetch
  }

  const handleDeleted = () => {
    closeDrawer()
    setGraphKey(k => k + 1)
  }

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

      {/* Graph fills remaining space */}
      <div className="flex-1 min-h-0">
        <BindingGraph key={graphKey} onPromptClick={openEdit} />
      </div>

      <PromptDrawer
        promptId={selectedId}
        isNew={isNew}
        onClose={closeDrawer}
        onSaved={handleSaved}
        onDeleted={handleDeleted}
      />
    </div>
  )
}
