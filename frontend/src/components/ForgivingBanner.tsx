import { motion } from 'motion/react'
import { AlertTriangle } from 'lucide-react'

interface ForgivingBannerProps {
  eventType: string
  onCreateRule: () => void
  onDismiss: () => void
}

export default function ForgivingBanner({
  eventType,
  onCreateRule,
  onDismiss,
}: ForgivingBannerProps) {
  return (
    <motion.div
      initial={{ opacity: 0, height: 0, marginBottom: 0 }}
      animate={{ opacity: 1, height: 'auto', marginBottom: 16 }}
      exit={{ opacity: 0, height: 0, marginBottom: 0 }}
      transition={{ duration: 0.2 }}
      className="overflow-hidden"
    >
      <div className="px-4 py-3 rounded-xl border border-warm/60 bg-warm/50 backdrop-blur-sm flex items-center gap-3">
        <AlertTriangle size={16} className="text-warm shrink-0" />
        <span className="text-body text-ink-2 flex-1">
          No rules or triggers are surfacing{' '}
          <strong className="font-mono text-ui">{eventType}</strong> events anymore.
        </span>
        <button
          onClick={onCreateRule}
          className="text-ui font-semibold text-warm hover:text-warm/80 transition-colors whitespace-nowrap"
        >
          Create task rule
        </button>
        <button
          onClick={onDismiss}
          className="text-ui text-ink-3 hover:text-ink-2 transition-colors whitespace-nowrap"
        >
          Dismiss
        </button>
      </div>
    </motion.div>
  )
}
