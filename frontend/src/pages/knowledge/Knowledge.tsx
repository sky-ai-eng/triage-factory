import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import Table from '../../ui/table/Table'
import type { TableRow } from '../../ui/table/Table'
import Dialog from '../../ui/dialog/Dialog'
import { usePageHeader } from '../../contexts/ChromeContext'
import { usePagedList } from '../../hooks/usePagedList'
import { useTeams } from '../../hooks/useTeams'
import { useTeamRole } from '../../hooks/useTeamRole'
import { useOptionalAuth } from '../../contexts/AuthContext'
import { useWebSocket } from '../../hooks/useWebSocket'
import { toast } from '../../components/Toast/toastStore'
import { httpErrorMessage } from '../../lib/apiClient'
import {
  deleteKnowledge,
  fmtBytes,
  knowledgeFileUrl,
  moveKnowledge,
  otherRoot,
  readKnowledgeFile,
  uploadKnowledge,
} from '../../lib/knowledge'
import type { KnowledgeFile, KnowledgeRoot } from '../../lib/knowledge'
import './knowledge.css'

// Knowledge — a team's knowledge base, and the one place a human curates what
// every delegated run reads.
//
// THE ROOT IS THE VISIBILITY. There is no per-file setting: a document under
// `private/` is readable through this team's gate alone, one under `shared/` by
// anyone in the organization, and publishing is moving it between them. So the
// page is two tabs and one verb, rather than a visibility column with a
// dropdown in it — the model and the interface are the same shape.
//
// WHAT THE PAGE IS FOR is stated on it. A knowledge base whose effect is
// invisible gets stale, and this one has a very specific effect: every run on a
// task belonging to this team is staged with all of it, plus every other team's
// published root, before the agent reads its first file. The line under the
// title says so, because otherwise nobody knows why to keep it current.
//
// WHAT IS CUT, deliberately, and not stubbed: browsing ANOTHER team's published
// root. The API serves it — the same routes on that team's id — but there is no
// org-wide team directory to offer as a switcher, and a picker listing only
// your own teams while claiming to show the org's published knowledge would be
// a lie. Other teams' published notes still reach every run; they are just not
// readable here yet. Renaming and reorganizing are the same move verb as
// publishing and are not surfaced either; re-uploading over a path replaces it.

/** The two tabs, in the order the roots are presented everywhere: where a team
 *  writes, then what it has published. */
const ROOT_TABS: Array<{ root: KnowledgeRoot; label: string; blurb: string }> = [
  {
    root: 'private',
    label: 'PRIVATE',
    blurb: 'Only this team can read these. Every run on this team’s tasks is staged with them.',
  },
  {
    root: 'shared',
    label: 'SHARED',
    blurb:
      'Published to the organization. Every team can read these, and every run in the org is staged with them.',
  },
]

/** A table row id: the whole address of the document the row draws, TEAM
 *  INCLUDED. The team is in here rather than read from a closure at send time
 *  because the Table defers a verb for the length of its undo window and then
 *  calls whatever `onCommit` is current — and the team switcher stays live
 *  throughout, since the undo bar is inline rather than modal. A commit that
 *  resolved the team lazily would fire against whichever team was selected ten
 *  seconds later, mutating a same-named document in a team the reader never
 *  touched, with no error and nothing on screen to show it. Binding it into the
 *  id makes that unrepresentable: the address a row was minted with is the
 *  address its verb commits to. */
function rowID(teamId: string, file: KnowledgeFile): string {
  return teamId + '/' + file.root + '/' + file.path
}

/** Split a row id back into the address it was minted from. Returns null for
 *  anything that is not one, so a malformed id is skipped rather than sent to a
 *  route that would refuse it. Team ids are uuids and roots are a closed
 *  two-value vocabulary, so the first two segments are unambiguous and
 *  everything after them is the path — which may itself contain separators. */
function splitRowID(id: string): { teamId: string; root: KnowledgeRoot; path: string } | null {
  const [teamId, root, ...rest] = id.split('/')
  const path = rest.join('/')
  if (!teamId || (root !== 'private' && root !== 'shared') || path === '') return null
  return { teamId, root, path }
}

/** The page's own window on the list read. Generous, because the whole point of
 *  the table is to see a team's estate at once; the server still pages, and the
 *  footer offers the next page when there is one. */
const PAGE_SIZE = 200

export default function Knowledge() {
  const { teams, lastActingTeamId, loaded } = useTeams()
  const { roleForTeam } = useTeamRole()
  // Membership roles are a multi-mode idea. Local is one person who is
  // everything, so the write verbs are simply on rather than gated against an
  // endpoint that has no roles to answer with.
  const isLocal = useOptionalAuth() === null

  const [picked, setPicked] = useState('')
  const team = useMemo(() => {
    if (!loaded) return null
    return (
      teams.find((t) => t.id === picked) ??
      teams.find((t) => t.id === lastActingTeamId) ??
      teams[0] ??
      null
    )
  }, [teams, picked, lastActingTeamId, loaded])
  const teamId = team?.id ?? ''
  const canWrite = isLocal || (teamId !== '' && roleForTeam(teamId) !== 'viewer')

  const [root, setRoot] = useState<KnowledgeRoot>('private')
  const [filter, setFilter] = useState('')
  const [viewing, setViewing] = useState<KnowledgeFile | null>(null)
  const [confirmUpload, setConfirmUpload] = useState<File[] | null>(null)
  const [uploadFolder, setUploadFolder] = useState('')
  const fileInput = useRef<HTMLInputElement | null>(null)

  const list = usePagedList<KnowledgeFile>(
    teamId ? `/api/teams/${encodeURIComponent(teamId)}/kb/list` : '',
    'Could not read this team’s knowledge base.',
  )
  const { load } = list

  const reload = useCallback(() => {
    if (!teamId) return
    void load({ root, page_size: PAGE_SIZE })
  }, [teamId, root, load])

  useEffect(() => {
    reload()
  }, [reload])

  // The invalidation ping. It names the team and the root that changed, so a
  // page showing one root does not refetch when the other one is written —
  // and a page showing another team does not refetch at all.
  useWebSocket(
    useCallback(
      (event) => {
        if (event.type !== 'team_knowledge_updated') return
        if (event.data.team_id !== teamId || event.data.root !== root) return
        reload()
      },
      [teamId, root, reload],
    ),
  )

  const rows: TableRow[] = useMemo(() => {
    const q = filter.trim().toLowerCase()
    return list.items
      .filter((f) => !q || f.path.toLowerCase().includes(q))
      .map((f) => ({
        id: rowID(teamId, f),
        path: f.path,
        size: fmtBytes(f.size),
        updated: f.mod_time,
        file: f,
      }))
  }, [list.items, filter, teamId])

  const send = useCallback(
    async (action: string, ids: Array<string | number>) => {
      // The row id IS the whole address, team included. Everything this commit
      // needs comes out of it and nothing out of the surrounding closure: the
      // undo window is ten seconds long, and in that time the held items can be
      // refetched (which would turn a committed verb into a silent no-op) and
      // the selected team can change (which would send it somewhere else
      // entirely).
      const refs = ids.map((id) => splitRowID(String(id))).filter((r) => r !== null)
      for (const ref of refs) {
        try {
          if (action === 'publish') {
            await moveKnowledge(
              ref.teamId,
              { root: ref.root, path: ref.path },
              { root: otherRoot(ref.root), path: ref.path },
            )
          } else {
            await deleteKnowledge(ref.teamId, ref.root, ref.path)
          }
        } catch (err) {
          toast.error(httpErrorMessage(err, 'The change could not be saved.'), ref.path)
        }
      }
      // A refetch either way: on success it reconciles the optimistic removal
      // against what the server actually holds, and on failure it puts back the
      // rows the table already took off the screen.
      if (refs.length > 0) reload()
    },
    [reload],
  )

  const runUpload = useCallback(
    async (files: File[], folder: string) => {
      try {
        const written = await uploadKnowledge(teamId, root, files, folder.trim())
        const replaced = written.filter((f) => f.outcome === 'replaced').length
        toast.success(
          replaced > 0
            ? `${written.length} file(s) saved — ${replaced} replaced an existing document.`
            : `${written.length} file(s) saved.`,
          'Knowledge updated',
        )
        reload()
      } catch (err) {
        toast.error(httpErrorMessage(err, 'The upload could not be saved.'), 'Upload failed')
        // An upload can fail part-way through a batch, so the listing is the
        // only honest answer about which documents actually landed.
        reload()
      }
    },
    [teamId, root, reload],
  )

  const teamName = team?.name ?? ''
  const header = useMemo(
    () => ({
      crumbs: [{ name: 'Knowledge' }],
      title: teamName ? `${teamName} knowledge` : 'Knowledge',
    }),
    [teamName],
  )
  usePageHeader(header)

  if (loaded && !team) {
    return (
      <p className="kb-empty">
        You are not on a team in this organization yet, so there is no knowledge base to show.
      </p>
    )
  }

  const tab = ROOT_TABS.find((t) => t.root === root) ?? ROOT_TABS[0]

  return (
    <div className="kb">
      <div className="kb-headgroup">
        <div className="kb-top">
          <div className="kb-id">
            <h1 className="kb-name">{team?.name ?? 'Knowledge'}</h1>
            <span className="kb-sub">
              What the agents read. Every run on this team’s tasks is staged with all of it, plus
              what other teams have published — before the agent opens a single file.
            </span>
          </div>
          {teams.length >= 2 ? (
            <label className="kb-switch">
              <span className="kb-switch-l">TEAM</span>
              <select
                className="kb-switch-s"
                value={teamId}
                onChange={(e) => {
                  setPicked(e.target.value)
                  // The document on screen belongs to the team being left. It
                  // is closed rather than refetched under the new team: the
                  // header names a path, not a team, so swapping the body in
                  // place would show one team's document under another's name
                  // with nothing to distinguish them.
                  setViewing(null)
                }}
              >
                {teams.map((t) => (
                  <option key={t.id} value={t.id}>
                    {t.name}
                  </option>
                ))}
              </select>
            </label>
          ) : null}
        </div>

        {/* The two roots are the visibility model, so they are the page's
            primary control rather than a filter tucked beside one. */}
        <div className="kb-tabs" role="tablist" aria-label="Visibility">
          {ROOT_TABS.map((t) => (
            <button
              key={t.root}
              type="button"
              role="tab"
              aria-selected={t.root === root}
              className="kb-tab"
              data-on={t.root === root ? '' : undefined}
              onClick={() => setRoot(t.root)}
            >
              {t.label}
            </button>
          ))}
          <span className="kb-tab-blurb">{tab.blurb}</span>
        </div>
      </div>

      <div className="kb-region">
        {list.error ? <p className="kb-error">{list.error}</p> : null}
        <Table
          build
          label={tab.label}
          columns={[
            {
              key: 'path',
              label: 'DOCUMENT',
              // The name is the way in. A row click belongs to selection —
              // that is what the checkbox column and the verbs are for — so
              // reading a document is its own control inside the cell, and it
              // stops the click rather than doing both at once.
              render: (row) => (
                <button
                  type="button"
                  className="kb-open"
                  onClick={(e) => {
                    e.stopPropagation()
                    setViewing(row.file as KnowledgeFile)
                  }}
                >
                  {row.path}
                </button>
              ),
              measure: (row) => String(row.path),
            },
            { key: 'size', label: 'SIZE', align: 'end', drop: 2 },
            { key: 'updated', label: 'UPDATED', type: 'ago', align: 'end', drop: 1 },
          ]}
          rows={rows}
          pageSize="auto"
          sortKey="path"
          barPosition="absolute"
          emptyLabel={
            list.loading
              ? 'Loading…'
              : root === 'private'
                ? 'Nothing filed yet. Upload the notes your team keeps re-explaining.'
                : 'Nothing published yet. Publish from Private to share with the organization.'
          }
          // Selection exists for the verbs, so a viewer sees neither.
          selectable={canWrite}
          headerRight={
            <input
              className="kb-filter"
              value={filter}
              onChange={(e) => setFilter(e.target.value)}
              placeholder="filter"
              aria-label="Filter documents"
            />
          }
          headerLeft={
            canWrite ? (
              <button
                type="button"
                className="kb-upload"
                onClick={() => fileInput.current?.click()}
              >
                + upload
              </button>
            ) : null
          }
          footer={
            list.hasMore ? (
              <button type="button" className="kb-more" onClick={() => void list.loadMore()}>
                Load more
              </button>
            ) : null
          }
          bar={
            canWrite
              ? {
                  actions: [
                    {
                      id: 'publish',
                      label: root === 'private' ? 'publish' : 'unpublish',
                      message: (n) =>
                        `${n} document${n === 1 ? '' : 's'} ${root === 'private' ? 'published to the organization' : 'made private again'}`,
                    },
                  ],
                  danger: {
                    label: 'Hold to delete',
                    action: {
                      id: 'delete',
                      label: 'delete',
                      message: (n) => `${n} document${n === 1 ? '' : 's'} deleted`,
                    },
                  },
                }
              : null
          }
          // Both verbs take the row out of THIS listing: a published document
          // moves to the other root, a deleted one is gone. Either way the row
          // leaves, and the reload afterwards is what reconciles it.
          mutate={() => null}
          onCommit={(action, ids) => void send(action, ids)}
        />
      </div>

      {/* The button's mechanism, not a control of its own — and absent, not
          hidden, for a reader who may not write: an affordance a viewer can
          reach by any route is an affordance that 403s. */}
      {canWrite ? (
        <input
          ref={fileInput}
          className="kb-file-input"
          type="file"
          multiple
          aria-hidden="true"
          tabIndex={-1}
          onChange={(e) => {
            const chosen = Array.from(e.target.files ?? [])
            // Reset the input so picking the same file twice in a row still fires.
            e.target.value = ''
            if (chosen.length === 0) return
            setUploadFolder('')
            setConfirmUpload(chosen)
          }}
        />
      ) : null}

      {/* Uploading states where the files land before it happens, because
          "which root" is the one thing that cannot be undone by re-uploading. */}
      <Dialog
        open={confirmUpload !== null}
        build="none"
        title={`Add to ${root}`}
        body={
          <div className="kb-upload-body">
            <p className="kb-upload-p">
              {confirmUpload?.length === 1
                ? confirmUpload[0].name
                : `${confirmUpload?.length ?? 0} files`}{' '}
              {root === 'private'
                ? 'will be readable by this team, and staged into this team’s runs.'
                : 'will be readable by the whole organization, and staged into every run in it.'}
            </p>
            <label className="kb-upload-f">
              <span className="kb-upload-fl">FOLDER (optional)</span>
              <input
                className="kb-upload-fi"
                value={uploadFolder}
                onChange={(e) => setUploadFolder(e.target.value)}
                placeholder="runbooks/deploys"
              />
            </label>
          </div>
        }
        confirmLabel="Add"
        onConfirm={() => {
          const files = confirmUpload ?? []
          const folder = uploadFolder
          setConfirmUpload(null)
          if (files.length > 0) void runUpload(files, folder)
        }}
        onCancel={() => setConfirmUpload(null)}
      />

      {viewing ? (
        // Keyed by the document, so opening a second one remounts the pane
        // with empty state rather than clearing the first one's text on the
        // way in — a reset written into the effect would be a cascading render
        // and, for the moment between the two, the wrong document's body under
        // the new one's name.
        <KnowledgeViewer
          key={viewing.root + '/' + viewing.path}
          teamId={teamId}
          file={viewing}
          onClose={() => setViewing(null)}
        />
      ) : null}
    </div>
  )
}

/** The reading pane. A knowledge base is for reading, so opening a document is
 *  a click rather than a download — the download link is still there, because a
 *  document that is not text is not readable here and says so. */
function KnowledgeViewer({
  teamId,
  file,
  onClose,
}: {
  teamId: string
  file: KnowledgeFile
  onClose: () => void
}) {
  const [text, setText] = useState<string | null>(null)
  const [error, setError] = useState('')

  useEffect(() => {
    const ctrl = new AbortController()
    readKnowledgeFile(teamId, file.root, file.path, ctrl.signal)
      .then(setText)
      .catch((err: unknown) => {
        if (ctrl.signal.aborted) return
        setError(httpErrorMessage(err, 'This document could not be read.'))
      })
    return () => ctrl.abort()
  }, [teamId, file.root, file.path])

  return (
    <div className="kb-view" role="dialog" aria-label={file.name}>
      <div className="kb-view-h">
        <span className="kb-view-t">{file.path}</span>
        <span className="kb-view-lead" />
        <a
          className="kb-view-dl"
          href={knowledgeFileUrl(teamId, file.root, file.path)}
          download={file.name}
        >
          download
        </a>
        <button type="button" className="kb-view-x" onClick={onClose} aria-label="Close">
          ×
        </button>
      </div>
      <div className="kb-view-b">
        {error ? (
          <p className="kb-error">{error}</p>
        ) : text === null ? (
          <p className="kb-view-wait">Loading…</p>
        ) : (
          <pre className="kb-view-pre">{text}</pre>
        )}
      </div>
    </div>
  )
}
