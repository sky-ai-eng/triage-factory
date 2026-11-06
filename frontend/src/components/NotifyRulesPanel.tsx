import { useState, useEffect, useCallback, useRef } from 'react'
import { motion, AnimatePresence } from 'motion/react'
import { X, Plus } from 'lucide-react'
import * as Switch from '@radix-ui/react-switch'
import EventBadge from './EventBadge'
import NotifyRuleEditor from './NotifyRuleEditor'
import type { NotifyHandler } from '../types'
import { toast } from './Toast/toastStore'
import { httpErrorMessage } from '../lib/apiClient'
import {
  getNotifyRule,
  notifyRulesListPath,
  personalNotifyRulesFilter,
  teamNotifyRulesFilter,
  updateNotifyRule,
} from '../lib/notifyRules'
import { useMe } from '../hooks/useDeploymentConfig'
import { useTeams } from '../hooks/useTeams'
import { usePagedList } from '../hooks/usePagedList'
import type { PagedList } from '../hooks/usePagedList'

// How many rules one page of a section carries; "Show more" reads the next.
const PAGE_SIZE = 50

const LOAD_ERROR = 'Could not load the notify rules.'

interface NotifyRulesPanelProps {
  open: boolean
  onClose: () => void
  // openRuleId opens that rule's editor once the rules have loaded, as a link
  // to it asks. Each linked id opens once, so a refetch after a save does not
  // open it again.
  openRuleId?: string | null
}

// NotifyRulesPanel lists the notify rules the viewer has a stake in, as two
// reads of the handler list: their own personal rules, and their teams' team
// rules. Another member's personal rule is readable through the API, as every
// handler of the team is, but it tells only its creator, so neither read asks
// for it.
export default function NotifyRulesPanel({ open, onClose, openRuleId }: NotifyRulesPanelProps) {
  const { me } = useMe()
  const { teams } = useTeams()
  const mineList = usePagedList<NotifyHandler>(notifyRulesListPath(), LOAD_ERROR)
  const teamList = usePagedList<NotifyHandler>(notifyRulesListPath(), LOAD_ERROR)
  const [editing, setEditing] = useState<NotifyHandler | null>(null)
  const [creating, setCreating] = useState(false)
  const [refreshKey, setRefreshKey] = useState(0)
  const openedLink = useRef<string | null>(null)

  const { load: loadMine, setItems: setMine } = mineList
  const { load: loadTeam, setItems: setTeam } = teamList
  const myId = me?.id
  useEffect(() => {
    if (!open) return
    void loadTeam({ ...teamNotifyRulesFilter(), page_size: PAGE_SIZE }, { keepDepth: true })
    // The personal read names its creator, so it waits for the caller's id.
    if (myId) {
      void loadMine(
        { ...personalNotifyRulesFilter(myId), page_size: PAGE_SIZE },
        { keepDepth: true },
      )
    }
  }, [open, refreshKey, myId, loadMine, loadTeam])

  const loadError = mineList.error || teamList.error
  useEffect(() => {
    if (loadError) toast.error(loadError)
  }, [loadError])

  // A link opens its rule once, read by id: the rule need not be on a page
  // either section has read.
  useEffect(() => {
    if (!openRuleId) {
      openedLink.current = null
      return
    }
    if (!open || openedLink.current === openRuleId) return
    openedLink.current = openRuleId
    const gone = 'That notify rule no longer exists, or belongs to a team you are not on.'
    getNotifyRule(openRuleId).then(
      (rule) => {
        if (openedLink.current !== openRuleId) return
        if (rule.kind === 'notify') setEditing(rule)
        else toast.error(gone)
      },
      (err) => toast.error(httpErrorMessage(err, gone)),
    )
  }, [open, openRuleId])

  const refresh = useCallback(() => setRefreshKey((k) => k + 1), [])

  // The server's gate, mirrored for presentation: a team rule is a team
  // admin's, a personal rule its creator's or a team admin's.
  const canWrite = useCallback(
    (rule: NotifyHandler) => {
      const admin = teams.find((t) => t.id === rule.team_id)?.role === 'admin'
      return admin || (rule.audience === 'creator' && rule.creator_user_id === me?.id)
    },
    [teams, me],
  )

  const toggleEnabled = useCallback(
    async (rule: NotifyHandler) => {
      // The section the rule was listed in holds it.
      const setRules = rule.audience === 'team' ? setTeam : setMine
      const prev = rule.enabled
      setRules((rs) => rs.map((r) => (r.id === rule.id ? { ...r, enabled: !prev } : r)))
      try {
        await updateNotifyRule(rule.id, { enabled: !prev })
      } catch (err) {
        setRules((rs) => rs.map((r) => (r.id === rule.id ? { ...r, enabled: prev } : r)))
        toast.error(httpErrorMessage(err, 'Could not toggle the notify rule.'))
      }
    },
    [setMine, setTeam],
  )

  const mine = mineList.items
  const team = teamList.items
  const loading = mineList.loading || teamList.loading
  const editorOpen = creating || editing !== null

  return (
    <AnimatePresence>
      {open && (
        <>
          <motion.div
            className="fixed inset-0 z-40"
            initial={{ opacity: 0, pointerEvents: 'none' as const }}
            animate={{ opacity: 1, pointerEvents: 'auto' as const }}
            exit={{ opacity: 0, pointerEvents: 'none' as const }}
            onClick={onClose}
          />
          <motion.div
            className="fixed top-20 right-4 bottom-4 z-50 w-[340px] bg-raised border border-line-1 rounded-2xl shadow-float shadow-black/[0.08] flex flex-col overflow-hidden"
            initial={{ x: '100%', opacity: 0 }}
            animate={{ x: 0, opacity: 1 }}
            exit={{ x: '100%', opacity: 0 }}
            transition={{ type: 'spring', damping: 28, stiffness: 300 }}
          >
            <div className="px-5 pt-5 pb-3 flex items-center justify-between shrink-0 border-b border-line-1">
              <h2 className="text-column font-semibold text-ink-1">Notify Rules</h2>
              <div className="flex items-center gap-2">
                <button
                  onClick={() => setCreating(true)}
                  className="flex items-center gap-1 text-ui font-semibold text-warm-ink bg-warm hover:bg-warm/90 px-3 py-1.5 rounded-full transition-colors"
                >
                  <Plus size={14} />
                  New
                </button>
                <button onClick={onClose} className="text-ink-3 hover:text-ink-2 transition-colors">
                  <X size={18} />
                </button>
              </div>
            </div>

            <div className="flex-1 overflow-y-auto p-3 space-y-4">
              {loading && mine.length === 0 && team.length === 0 && (
                <div className="space-y-2 p-2">
                  {[1, 2].map((i) => (
                    <div key={i} className="h-14 rounded-xl bg-tint-2 animate-pulse" />
                  ))}
                </div>
              )}
              {!loading && mine.length === 0 && team.length === 0 && (
                <p className="text-body text-ink-3 text-center py-12 px-4">
                  No notify rules yet. Create one to be told when an event happens, such as CI
                  failing on your pull request.
                </p>
              )}
              {[
                { title: 'Yours', list: mineList },
                { title: 'Team', list: teamList },
              ].map(
                ({ title, list }) =>
                  list.items.length > 0 && (
                    <section key={title}>
                      <h3 className="px-3 pb-1 text-label font-medium text-ink-3 uppercase tracking-wide">
                        {title}
                      </h3>
                      {list.items.map((rule) => (
                        <NotifyRuleRow
                          key={rule.id}
                          rule={rule}
                          writable={canWrite(rule)}
                          onEdit={() => setEditing(rule)}
                          onToggle={() => toggleEnabled(rule)}
                        />
                      ))}
                      <ShowMore
                        list={list}
                        label={title === 'Yours' ? 'your rules' : 'team rules'}
                      />
                    </section>
                  ),
              )}
            </div>
          </motion.div>

          <NotifyRuleEditor
            open={editorOpen}
            rule={editing}
            writable={editing ? canWrite(editing) : true}
            onClose={() => {
              setEditing(null)
              setCreating(false)
            }}
            onSaved={() => {
              setEditing(null)
              setCreating(false)
              refresh()
            }}
            onDeleted={refresh}
          />
        </>
      )}
    </AnimatePresence>
  )
}

/** ShowMore reads a section's next page, and renders nothing once the section
 *  holds its last one. */
function ShowMore({ list, label }: { list: PagedList<NotifyHandler>; label: string }) {
  if (!list.hasMore) return null
  return (
    <button
      onClick={() => void list.loadMore()}
      disabled={list.loading}
      aria-label={`Show more ${label}`}
      className="w-full px-3 py-2 text-left text-ui text-warm hover:text-warm/70 transition-colors disabled:opacity-50"
    >
      Show more
    </button>
  )
}

function NotifyRuleRow({
  rule,
  writable,
  onEdit,
  onToggle,
}: {
  rule: NotifyHandler
  writable: boolean
  onEdit: () => void
  onToggle: () => void
}) {
  const unavailable = rule.source_available === false
  return (
    <button
      onClick={onEdit}
      className="w-full text-left px-3 py-3 rounded-xl hover:bg-tint-2 transition-colors"
    >
      <div className="flex items-start justify-between gap-3 min-w-0">
        <div className="min-w-0 flex-1">
          <span
            className={`block text-body font-medium truncate ${
              rule.enabled && !unavailable ? 'text-ink-1' : 'text-ink-3 line-through'
            }`}
          >
            {rule.name}
          </span>
          <div className="mt-1">
            <EventBadge eventType={rule.event_type} compact />
          </div>
        </div>
        <div onClick={(e) => e.stopPropagation()} className="shrink-0 mt-0.5">
          <Switch.Root
            checked={rule.enabled}
            onCheckedChange={onToggle}
            disabled={!writable || unavailable}
            aria-label={`Enable ${rule.name}`}
            className="relative w-8 h-[18px] rounded-full transition-colors data-[state=checked]:bg-warm data-[state=unchecked]:bg-line-2 cursor-pointer disabled:cursor-not-allowed disabled:opacity-40"
          >
            <Switch.Thumb className="block w-[14px] h-[14px] rounded-full bg-raised shadow transition-transform data-[state=checked]:translate-x-[14px] data-[state=unchecked]:translate-x-[2px]" />
          </Switch.Root>
        </div>
      </div>
    </button>
  )
}
