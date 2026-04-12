import { useState, useEffect } from 'react'
import { motion, AnimatePresence } from 'motion/react'
import type { Prompt } from '../types'

interface Props {
  open: boolean
  onSelect: (promptId: string) => void
  onClose: () => void
  onEditPrompts: () => void
}

export default function PromptPicker({ open, onSelect, onClose, onEditPrompts }: Props) {
  const [prompts, setPrompts] = useState<Prompt[]>([])
  const [fetchFailed, setFetchFailed] = useState(false)
  // Derived: "loading" means open, no prompts cached yet, AND the last fetch
  // hasn't failed. The fetchFailed flag is what breaks us out of the skeleton
  // on error — without it, a failed fetch leaves prompts=[] and loading=true
  // forever. After a successful fetch, subsequent opens show cached prompts
  // instantly (intentional).
  const loading = open && prompts.length === 0 && !fetchFailed

  useEffect(() => {
    if (!open) return
    let cancelled = false
    fetch('/api/prompts')
      .then((res) => res.json())
      .then((data: Prompt[]) => {
        if (!cancelled) {
          setPrompts(data)
          setFetchFailed(false)
        }
      })
      .catch(() => {
        if (!cancelled) setFetchFailed(true)
      })
    return () => {
      cancelled = true
    }
  }, [open])

  return (
    <AnimatePresence>
      {open && (
        <>
          {/* Backdrop */}
          <motion.div
            className="fixed inset-0 bg-black/20 backdrop-blur-sm z-50"
            initial={{ opacity: 0 }}
            animate={{ opacity: 1 }}
            exit={{ opacity: 0 }}
            onClick={onClose}
          />

          {/* Picker panel */}
          <motion.div
            className="fixed inset-0 z-50 flex items-center justify-center pointer-events-none"
            initial={{ opacity: 0, scale: 0.95 }}
            animate={{ opacity: 1, scale: 1 }}
            exit={{ opacity: 0, scale: 0.95 }}
            transition={{ duration: 0.15 }}
          >
            <div className="pointer-events-auto bg-surface-raised/95 backdrop-blur-2xl border border-border-glass rounded-2xl shadow-2xl shadow-black/10 w-[480px] max-h-[400px] flex flex-col overflow-hidden">
              {/* Header */}
              <div className="px-5 pt-5 pb-3 flex items-center justify-between shrink-0">
                <div>
                  <h2 className="text-[15px] font-semibold text-text-primary">Choose a prompt</h2>
                  <p className="text-[12px] text-text-tertiary mt-0.5">
                    Select a delegation strategy for this task
                  </p>
                </div>
                <button
                  onClick={onClose}
                  className="text-text-tertiary hover:text-text-secondary transition-colors text-lg leading-none px-1"
                >
                  &times;
                </button>
              </div>

              {/* Tiles grid — scrollable */}
              <div className="px-5 pb-4 overflow-y-auto flex-1 min-h-0">
                {loading ? (
                  <div className="grid grid-cols-2 gap-3">
                    {[...Array(4)].map((_, i) => (
                      <div key={i} className="h-[88px] rounded-xl bg-black/[0.03] animate-pulse" />
                    ))}
                  </div>
                ) : (
                  <div className="grid grid-cols-2 gap-3">
                    {prompts.map((prompt) => (
                      <button
                        key={prompt.id}
                        onClick={() => onSelect(prompt.id)}
                        className="group text-left p-4 rounded-xl border border-border-subtle bg-white/50 hover:bg-white/80 hover:border-accent/30 hover:shadow-sm transition-all duration-150"
                      >
                        <div className="flex items-center gap-2 mb-1.5">
                          <span className="text-[13px] font-semibold text-text-primary group-hover:text-accent transition-colors truncate">
                            {prompt.name}
                          </span>
                          {prompt.source === 'system' && (
                            <span className="text-[9px] font-semibold uppercase tracking-wider px-1.5 py-0.5 rounded bg-black/[0.04] text-text-tertiary shrink-0">
                              System
                            </span>
                          )}
                        </div>
                        <p className="text-[11px] text-text-tertiary line-clamp-2 leading-relaxed">
                          {prompt.body.slice(0, 120)}
                          {prompt.body.length > 120 ? '...' : ''}
                        </p>
                        {prompt.usage_count > 0 && (
                          <span className="text-[10px] text-text-tertiary mt-1.5 inline-block">
                            Used {prompt.usage_count}x
                          </span>
                        )}
                      </button>
                    ))}

                    {/* Add new tile */}
                    <button
                      onClick={onEditPrompts}
                      className="flex flex-col items-center justify-center p-4 rounded-xl border border-dashed border-border-subtle hover:border-accent/40 hover:bg-accent/[0.03] transition-all duration-150 min-h-[88px]"
                    >
                      <span className="text-2xl text-text-tertiary leading-none mb-1">+</span>
                      <span className="text-[11px] text-text-tertiary">New Prompt</span>
                    </button>
                  </div>
                )}
              </div>

              {/* Footer */}
              <div className="px-5 py-3 border-t border-border-subtle flex items-center justify-between shrink-0">
                <button
                  onClick={onEditPrompts}
                  className="text-[12px] text-accent hover:text-accent/70 font-medium transition-colors"
                >
                  Edit Prompts
                </button>
                <span className="text-[11px] text-text-tertiary">Swipe up to delegate</span>
              </div>
            </div>
          </motion.div>
        </>
      )}
    </AnimatePresence>
  )
}
