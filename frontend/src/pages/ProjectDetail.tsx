import { useState, useEffect, useCallback, useMemo, useRef } from 'react'
import { useParams, useNavigate, Link } from 'react-router'
import * as Popover from '@radix-ui/react-popover'
import {
  ArrowLeft,
  Trash2,
  Pencil,
  Check,
  X,
  Plus,
  ExternalLink,
  FileText,
  Image as ImageIcon,
  File as FileIcon,
  Download,
  Loader2,
} from 'lucide-react'
import Markdown from 'react-markdown'
import type {
  Project,
  ProjectVisibility,
  RepoOption,
  KnowledgeFile,
  KnowledgeUploadResult,
  ProjectExportPreview,
} from '../types'
import { apiFetch, apiJSON, apiListAll, HttpError, httpErrorMessage } from '../lib/apiClient'
import { useFocusTrap } from '../hooks/useFocusTrap'
import { toast } from '../components/Toast/toastStore'
import TrackerProjectPickers from '../components/TrackerProjectPickers'
import ProjectVisibilitySelect from '../components/ProjectVisibilitySelect'
import ProjectEntitiesPanel from '../components/ProjectEntitiesPanel'
import { useWebSocket } from '../hooks/useWebSocket'
import { useOrgHref } from '../hooks/useOrgHref'
import { useOrgRole } from '../hooks/useOrgRole'
import { useTeamRole } from '../hooks/useTeamRole'
import { useDeploymentConfig, useMe } from '../hooks/useDeploymentConfig'

// ProjectDetail is the per-project workspace. Top-to-bottom on the
// left:
//   1. Header — name + description (inline-editable), pinned repos
//      surfaced as interactive chips alongside tracker chips.
//   2. Integrations — Jira / Linear pickers. Pinned repos lived here
//      in an earlier draft but moved into the header so the user
//      doesn't see two surfaces showing the same data.
//   3. Knowledge base — markdown files under the project's
//      knowledge-base directory, rendered read-only.
//
// Edits across the page are auto-saved — there's no explicit Save button.
// The patch helper handles error toasts; on success the page resyncs from
// the freshly-returned project row.
export default function ProjectDetail() {
  const { id } = useParams<{ id: string }>()
  const navigate = useNavigate()
  const orgHref = useOrgHref()
  const [project, setProject] = useState<Project | null>(null)
  const [loading, setLoading] = useState(true)
  const [missing, setMissing] = useState(false)
  const [exportOpen, setExportOpen] = useState(false)
  // loadError distinguishes "really gone" (404 → missing=true) from
  // "transient failure" (5xx, network drop). Without it, a flaky
  // network request would land in the missing branch and the user
  // would see "Project not found" for a project that very much
  // still exists.
  const [loadError, setLoadError] = useState<string | null>(null)
  // Two seqs: patchSeq increments on every PATCH issued; lastLandedSeq
  // tracks the highest seq whose response actually got applied. The
  // "skip stale response" check uses lastLandedSeq, not patchSeq, so a
  // newer-but-failed PATCH doesn't suppress an earlier-but-successful
  // one. Without this split, two concurrent autosaves where the later
  // one returned 4xx would leave the page rendering pre-edit data even
  // though the earlier edit persisted server-side.
  const patchSeq = useRef(0)
  const lastLandedSeq = useRef(0)
  // currentIDRef holds the live id so PATCH callbacks (whose closure
  // captures id at issue time) can check whether the user has navigated
  // to a different project before toasting an error or applying state.
  // Without this, comparing `myID === id` inside the closure compares
  // the captured id to itself — always true — and a PATCH-error toast
  // for project A still fires while the user is looking at project B.
  const currentIDRef = useRef<string | undefined>(id)

  const loadProject = useCallback(
    async (signal: AbortSignal) => {
      if (!id) return
      try {
        const data = await apiJSON<Project>(`/api/projects/${encodeURIComponent(id)}`, { signal })
        if (signal.aborted) return
        setProject(data)
      } catch (err) {
        if (signal.aborted) return
        // A 404 is this project's own not-found state, not a failed read.
        if (err instanceof HttpError && err.status === 404) {
          setMissing(true)
          return
        }
        setLoadError(httpErrorMessage(err, 'Could not load the project.'))
      } finally {
        if (!signal.aborted) setLoading(false)
      }
    },
    [id],
  )

  // Load on mount and on id change. Resetting visible state at the
  // top of the effect avoids a flash of the previous project's data
  // when navigating between /projects/:id pages — React Router can
  // reuse the component, so without the reset we briefly render the
  // old project until the new fetch lands.
  //
  // AbortController gates state updates against out-of-order
  // responses: if A→B→C navigation fires three fetches and they
  // resolve in the wrong order, only the latest effect's setState
  // path survives (each prior cleanup aborts its controller).
  //
  // We stash the controller in a ref so the retry button (rendered
  // outside this effect's scope) can swap in its own controller AND
  // have it aborted on subsequent navigation. Without the ref, a
  // user who hits retry then navigates away would leave the retry's
  // fetch alive — its setProject would land for the wrong id.
  const loadAbortRef = useRef<AbortController | null>(null)

  useEffect(() => {
    if (!id) return
    setProject(null)
    setMissing(false)
    setLoadError(null)
    setLoading(true)
    // Update the live-id ref synchronously so any in-flight PATCH
    // closures that compare against currentIDRef.current see the
    // new id and bail before toasting / setProject for the project
    // they were issued against.
    currentIDRef.current = id
    // Bump patchSeq + reset lastLandedSeq on id change so any
    // in-flight PATCH responses from project A find their mySeq
    // already overtaken when they land — they can't accidentally
    // setProject(A) over project B's freshly-loaded state.
    patchSeq.current += 1
    lastLandedSeq.current = patchSeq.current
    const controller = new AbortController()
    loadAbortRef.current = controller
    loadProject(controller.signal)
    return () => {
      controller.abort()
      // Clear the ref only if it still points at our controller —
      // the retry path may have swapped in a fresh one and we
      // don't want to step on its lifecycle.
      if (loadAbortRef.current === controller) {
        loadAbortRef.current = null
      }
    }
  }, [id, loadProject])

  const patch = useCallback(
    async (body: Record<string, unknown>) => {
      if (!id) return false
      // Capture id + seq BEFORE the await. Both gates run at apply
      // time:
      //   - id gate: if the user navigated to a different project,
      //     this PATCH was issued against a different id and must
      //     not setProject — that would replace project B's state
      //     with project A's row, and any subsequent autosave
      //     would merge A's data back into B.
      //   - seq gate: lastLandedSeq tracks the highest seq we've
      //     actually rendered, so older successful responses can't
      //     overwrite a newer one and a newer-failed sibling can't
      //     suppress an older-successful response.
      const myID = id
      patchSeq.current += 1
      const mySeq = patchSeq.current
      try {
        const fresh = await apiJSON<Project>(`/api/projects/${encodeURIComponent(myID)}`, {
          method: 'PATCH',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(body),
        })
        // Compare captured myID against the LIVE current id (via ref)
        // rather than the closure's `id` — the closure captured the
        // same value as myID, so `myID === id` would always be true
        // and the guard wouldn't actually protect against navigation.
        if (myID === currentIDRef.current && mySeq > lastLandedSeq.current) {
          lastLandedSeq.current = mySeq
          setProject(fresh)
        }
        return true
      } catch (err) {
        if (myID === currentIDRef.current) {
          toast.error(httpErrorMessage(err, 'Could not update the project.'))
        }
        return false
      }
    },
    [id],
  )

  const handleDelete = useCallback(async () => {
    if (!id || !project) return
    if (!confirm(`Delete project "${project.name}"? This can't be undone.`)) return
    try {
      const res = await apiFetch(`/api/projects/${encodeURIComponent(id)}`, { method: 'DELETE' })
      const cleanupWarning = res.headers.get('X-Cleanup-Warning')
      if (cleanupWarning) {
        toast.warning(cleanupWarning)
      } else {
        toast.success(`Deleted project "${project.name}"`)
      }
      navigate(orgHref('/projects'))
    } catch (err) {
      toast.error(httpErrorMessage(err, 'Could not delete the project.'))
    }
  }, [id, project, navigate, orgHref])

  if (loading) {
    return (
      <div className="max-w-7xl mx-auto">
        <div className="text-ink-3 text-body">Loading project…</div>
      </div>
    )
  }

  // Distinguish three "no project to render" cases so the user
  // gets accurate feedback rather than a generic "not found":
  //   - missing: the API returned 404. Project really is gone.
  //   - loadError: a non-404 failure (5xx, network). Show retry.
  //   - !project: bare null with no error and no missing. Shouldn't
  //     happen normally; treat like a transient error.
  if (missing) {
    return (
      <div className="max-w-7xl mx-auto">
        <Link
          to={orgHref('/projects')}
          className="inline-flex items-center gap-1 text-body text-ink-2 hover:text-ink-1 mb-6"
        >
          <ArrowLeft size={14} /> Projects
        </Link>
        <div className="text-ink-2 text-body">Project not found. It may have been deleted.</div>
      </div>
    )
  }

  if (loadError || !project) {
    return (
      <div className="max-w-7xl mx-auto">
        <Link
          to={orgHref('/projects')}
          className="inline-flex items-center gap-1 text-body text-ink-2 hover:text-ink-1 mb-6"
        >
          <ArrowLeft size={14} /> Projects
        </Link>
        <div className="text-ink-2 text-body mb-3">{loadError ?? 'Failed to load project.'}</div>
        <button
          type="button"
          onClick={() => {
            setLoadError(null)
            setLoading(true)
            // Abort any prior in-flight load (e.g. the original
            // load we're retrying after) and register the new
            // controller so subsequent navigation can abort it
            // through the same ref the effect uses. Without this,
            // a retry started right before navigating away keeps
            // running and its setProject lands for the wrong id.
            loadAbortRef.current?.abort()
            const controller = new AbortController()
            loadAbortRef.current = controller
            loadProject(controller.signal)
          }}
          className="
            inline-flex items-center gap-1.5 rounded-full
            bg-warm text-warm-ink text-body font-medium
            px-4 py-1.5 hover:opacity-90
          "
        >
          Try again
        </button>
      </div>
    )
  }

  return (
    <div className="max-w-7xl mx-auto">
      <div className="flex items-center justify-between mb-6">
        <Link
          to={orgHref('/projects')}
          className="inline-flex items-center gap-1 text-body text-ink-2 hover:text-ink-1"
        >
          <ArrowLeft size={14} /> Projects
        </Link>
        <div className="flex items-center gap-2">
          <button
            type="button"
            onClick={() => setExportOpen(true)}
            className="
              inline-flex items-center gap-1.5 rounded-full
              px-3 py-1.5 text-ui
              text-ink-2 border border-line-1 bg-raised
              hover:text-ink-1 hover:bg-raised transition-all
            "
          >
            <Download size={12} />
            Export
          </button>
          <button
            type="button"
            onClick={handleDelete}
            className="
              inline-flex items-center gap-1.5 rounded-full
              px-3 py-1.5 text-ui
              text-alarm/80 hover:text-alarm hover:bg-alarm/[0.08]
              transition-all
            "
          >
            <Trash2 size={12} />
            Delete project
          </button>
        </div>
      </div>

      <div className="space-y-6">
        <ProjectHeader
          project={project}
          onPatchName={(name) => patch({ name })}
          onPatchDescription={(description) => patch({ description })}
          onPatchPinnedRepos={(ids) => patch({ pinned_repository_ids: ids })}
        />

        <VisibilityPanel project={project} onPatch={patch} />

        {/* A teamless private/org project (TFAC-562) has no team to
            validate a Jira/Linear key against in v1 — the header's
            pinned-repos hint already explains that, so skip a second,
            redundant empty card here rather than duplicating it. */}
        {project.team_id && <IntegrationsPanel project={project} onPatch={patch} />}

        <KnowledgePanel projectId={project.id} />

        <ProjectEntitiesPanel projectId={project.id} />
      </div>
      {exportOpen && (
        <ProjectExportModal
          projectId={project.id}
          projectName={project.name}
          onClose={() => setExportOpen(false)}
        />
      )}
    </div>
  )
}

// ProjectHeader handles inline edit for name + description and embeds
// the pinned-repos editor + tracker chips in one cohesive block. The
// pinned-repos chips are interactive: hover surfaces an X to remove,
// and a "+" affordance opens a popover of remaining configured repos
// to add. Auto-saves on change.
function ProjectHeader({
  project,
  onPatchName,
  onPatchDescription,
  onPatchPinnedRepos,
}: {
  project: Project
  onPatchName: (name: string) => Promise<boolean | undefined>
  onPatchDescription: (description: string) => Promise<boolean | undefined>
  onPatchPinnedRepos: (repositoryIDs: string[]) => Promise<boolean | undefined>
}) {
  const [editingName, setEditingName] = useState(false)
  const [editingDesc, setEditingDesc] = useState(false)
  const [draftName, setDraftName] = useState(project.name)
  const [draftDesc, setDraftDesc] = useState(project.description)

  const beginEditName = () => {
    setDraftName(project.name)
    setEditingName(true)
  }

  const beginEditDesc = () => {
    setDraftDesc(project.description)
    setEditingDesc(true)
  }

  const saveName = async () => {
    if (!draftName.trim() || draftName.trim() === project.name) {
      setEditingName(false)
      return
    }
    const ok = await onPatchName(draftName.trim())
    if (ok) setEditingName(false)
  }

  const saveDesc = async () => {
    if (draftDesc === project.description) {
      setEditingDesc(false)
      return
    }
    const ok = await onPatchDescription(draftDesc)
    if (ok) setEditingDesc(false)
  }

  return (
    <Card>
      <div className="flex items-start justify-between gap-3">
        {editingName ? (
          <div className="flex-1 flex items-center gap-2">
            <input
              type="text"
              value={draftName}
              onChange={(e) => setDraftName(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === 'Enter') saveName()
                if (e.key === 'Escape') {
                  setDraftName(project.name)
                  setEditingName(false)
                }
              }}
              autoFocus
              className="
                flex-1 rounded-lg border border-line-1
                bg-raised px-3 py-1.5 text-lg font-semibold tracking-tight
                text-ink-1
                focus:outline-none focus:border-warm
              "
            />
            <button
              type="button"
              onClick={saveName}
              aria-label="Save project name"
              className="text-ink-2 hover:bg-tint-2 p-1.5 rounded-full"
            >
              <Check size={14} />
            </button>
            <button
              type="button"
              onClick={() => {
                setDraftName(project.name)
                setEditingName(false)
              }}
              aria-label="Cancel editing project name"
              className="text-ink-3 hover:bg-tint-2 p-1.5 rounded-full"
            >
              <X size={14} />
            </button>
          </div>
        ) : (
          <h1 className="text-2xl font-semibold tracking-tight text-ink-1">
            <button
              type="button"
              onClick={beginEditName}
              className="group inline-flex items-center gap-2 text-inherit cursor-pointer"
            >
              {project.name}
              <Pencil size={12} className="text-ink-3 opacity-0 group-hover:opacity-100" />
            </button>
          </h1>
        )}
      </div>

      <div className="mt-3">
        {editingDesc ? (
          <div className="space-y-2">
            <textarea
              value={draftDesc}
              onChange={(e) => setDraftDesc(e.target.value)}
              autoFocus
              rows={3}
              className="
                w-full rounded-lg border border-line-1
                bg-raised px-3 py-2 text-body text-ink-1
                resize-none focus:outline-none focus:border-warm
              "
            />
            <div className="flex justify-end gap-2">
              <button
                type="button"
                onClick={() => {
                  setDraftDesc(project.description)
                  setEditingDesc(false)
                }}
                className="text-ui text-ink-2 hover:text-ink-1 px-2 py-1 rounded-full"
              >
                Cancel
              </button>
              <button
                type="button"
                onClick={saveDesc}
                className="text-ui bg-warm text-warm-ink px-3 py-1 rounded-full hover:opacity-90"
              >
                Save
              </button>
            </div>
          </div>
        ) : (
          // Wrapped in a real <button> so keyboard users can tab to
          // it and press Enter/Space to begin editing — the earlier
          // <p onClick> path was mouse-only. text-left + items-start
          // preserve the rendered look of the original paragraph.
          <button
            type="button"
            onClick={beginEditDesc}
            className="
              text-left text-body text-ink-2 leading-relaxed
              cursor-pointer group inline-flex items-start gap-2
              hover:text-ink-1 focus:outline-none
              focus-visible:ring-2 focus-visible:ring-warm rounded
            "
          >
            {project.description ? (
              project.description
            ) : (
              <span className="italic text-ink-3">Add a description…</span>
            )}
            <Pencil
              size={12}
              className="text-ink-3 opacity-0 group-hover:opacity-100 mt-1 shrink-0"
            />
          </button>
        )}
      </div>

      <div className="mt-4">
        {project.team_id ? (
          <PinnedReposInline
            pinned={project.pinned_repository_ids}
            teamId={project.team_id}
            onChange={onPatchPinnedRepos}
            jiraKey={project.jira_project_key}
            linearKey={project.linear_project_key}
          />
        ) : (
          // A private/org-visibility project created teamless has no team
          // to validate pinned repos or tracker keys against in v1 (see
          // handleProjectUpdate's matching 400s) — mirrors the same copy
          // ProjectCreateModal shows when those fields are hidden there.
          <p className="text-ui text-ink-3 italic">
            Pinned repos and tracker projects are available for team-visibility projects.
          </p>
        )}
      </div>
    </Card>
  )
}

// PinnedReposInline renders the pinned-repo chips alongside tracker
// chips. Pinned chips are interactive: hovering surfaces an X to
// remove the pin (auto-saved), and a trailing "+" button opens a
// popover that lists remaining configured repos to add.
//
// It holds repository ids throughout — `pinned`, `local`, and everything
// the PATCH sends — and reads a name only to put one on a chip. That is
// what makes a rename mid-session harmless: the ids the page is holding
// go on addressing the same repositories, and the names refresh on the
// next load.
//
// The tracker chips render inline but aren't editable here — that's
// the IntegrationsPanel's job. Co-locating them visually keeps the
// "this project is X plus these things" narrative tight.
function PinnedReposInline({
  pinned,
  teamId,
  onChange,
  jiraKey,
  linearKey,
}: {
  /** Registry row ids — see Project.pinned_repository_ids. */
  pinned: string[]
  teamId: string
  onChange: (next: string[]) => Promise<boolean | undefined>
  jiraKey: string
  linearKey: string
}) {
  const orgHref = useOrgHref()
  const [available, setAvailable] = useState<RepoOption[]>([])
  const [loading, setLoading] = useState(true)
  const [loadError, setLoadError] = useState<string | null>(null)
  const [adderOpen, setAdderOpen] = useState(false)
  const [search, setSearch] = useState('')

  // Local intended state, updated synchronously on every user
  // action. Without this, two quick clicks (e.g. remove A, then
  // remove B before A's PATCH lands) compose against the stale
  // `pinned` prop and the second click sends [B] instead of [],
  // dropping the first removal.
  const [local, setLocal] = useState<string[]>(pinned)

  // pendingTarget holds the most recent intended state while a
  // PATCH is in flight. The drain loop fires only the latest target
  // — intermediate states get coalesced. This mirrors the typical
  // "fire latest desired state" pattern: we don't care that
  // intermediate states ever hit the server, only that the final
  // one does.
  const pendingTarget = useRef<string[] | null>(null)
  const inflight = useRef(false)

  // Re-sync local from the prop only when no PATCH is outstanding.
  // While we have pending edits, the prop reflects pre-edit server
  // state and would clobber the user's in-progress changes.
  useEffect(() => {
    if (!inflight.current && pendingTarget.current === null) {
      setLocal(pinned)
    }
  }, [pinned])

  // loadRepos populates `available` with the project's OWN team's tracked
  // repos — the same set the PATCH validator accepts — carrying the id to
  // submit and the name to show. It takes both reads because the two facts
  // live apart: tracking is name-shaped (it is the edge that mints a
  // registry row), and the registry is what carries the ids. Offering the
  // registry alone would include sibling teams' repos this project's team
  // doesn't track, so adding one would 400. Tracks loadError separately so
  // a transient failure surfaces as a "couldn't load — try again" hint in
  // the popover instead of the misleading "No repos configured" empty
  // state, which would route the user to a setup page they may have
  // already completed.
  const loadRepos = useCallback(
    async (signal: AbortSignal) => {
      setLoadError(null)
      // Empty team_id is a legitimate state since TFAC-562 (a private/org-
      // visibility project has no team) — the caller (ProjectHeader) only
      // mounts this component when project.team_id is set, so this guard
      // is unreachable in practice. It stays as defense in depth: fetching
      // `/api/teams//github-repos` would 404 and bury the real cause
      // behind a generic load error if that invariant is ever violated.
      if (!teamId) {
        setLoadError('This project has no team — pinned repos are unavailable.')
        setLoading(false)
        return
      }
      try {
        const [team, registry] = await Promise.all([
          apiJSON<{ repos?: string[] }>(`/api/teams/${teamId}/github-repos`, { signal }),
          apiListAll<RepoOption>('/api/repos/list', {}, { signal }),
        ])
        if (signal.aborted) return
        // Fold both sides: GitHub names are case-insensitive and the two
        // tables can hold different casings of one repository.
        const tracked = new Set((team.repos ?? []).map((slug) => slug.toLowerCase()))
        setAvailable(
          registry
            .filter((r) => tracked.has(r.slug.toLowerCase()))
            .sort((a, b) => a.slug.localeCompare(b.slug)),
        )
      } catch (err) {
        if (signal.aborted) return
        const message = httpErrorMessage(err, 'Could not load the tracked repos.')
        setLoadError(message)
        toast.error(message)
      } finally {
        if (!signal.aborted) setLoading(false)
      }
    },
    [teamId],
  )

  useEffect(() => {
    const controller = new AbortController()
    loadRepos(controller.signal)
    return () => controller.abort()
  }, [loadRepos])

  // applyChange queues a target state and drains. Concurrent calls
  // collapse: if the user clicks four removes quickly, the first
  // PATCH fires immediately and only the final intent fires after
  // it returns — intermediate states are skipped because they
  // weren't the user's final answer.
  const applyChange = async (next: string[]) => {
    setLocal(next)
    pendingTarget.current = next
    if (inflight.current) return
    inflight.current = true
    try {
      while (pendingTarget.current !== null) {
        const target = pendingTarget.current
        pendingTarget.current = null
        const ok = await onChange(target)
        if (!ok) {
          // Parent already toasted the error. Roll back to the
          // last known server state and drop any further pending
          // intents — keeping them queued would re-send a
          // probably-still-invalid state.
          pendingTarget.current = null
          setLocal(pinned)
          break
        }
      }
    } finally {
      inflight.current = false
    }
  }

  const remove = (id: string) => {
    applyChange(local.filter((pinnedID) => pinnedID !== id))
  }

  const add = (id: string) => {
    if (local.includes(id)) return
    applyChange([...local, id])
    // Close the picker optimistically — the PATCH may still be
    // in flight, but the user's intent ("add this") is captured
    // in pendingTarget and the chip already shows in `local`.
    setAdderOpen(false)
    setSearch('')
  }

  // Chips read their name out of the offered set. An id with no entry is a
  // repo the team stopped tracking since it was pinned: it still renders,
  // so the user can see and remove it, labelled with the id because that
  // is the only thing about it this component knows.
  const nameByID = useMemo(() => {
    const m = new Map<string, string>()
    for (const r of available) m.set(r.id, r.slug)
    return m
  }, [available])
  // Sorted by what the user reads, not by the opaque id.
  const chips = useMemo(
    () =>
      local
        .map((id) => ({ id, slug: nameByID.get(id) ?? id }))
        .sort((a, b) => a.slug.localeCompare(b.slug)),
    [local, nameByID],
  )

  const addable = available.filter(
    (r) =>
      !local.includes(r.id) &&
      (!search.trim() || r.slug.toLowerCase().includes(search.trim().toLowerCase())),
  )

  return (
    <div className="flex flex-wrap items-center gap-1.5">
      {jiraKey && <Chip label={`Jira: ${jiraKey}`} tone="accent" />}
      {linearKey && <Chip label={`Linear: ${linearKey}`} tone="accent" />}
      {chips.map((r) => (
        <RepoChip key={r.id} slug={r.slug} onRemove={() => remove(r.id)} />
      ))}
      <Popover.Root open={adderOpen} onOpenChange={setAdderOpen}>
        <Popover.Trigger asChild>
          <button
            type="button"
            className="
              inline-flex items-center gap-1 rounded-full
              border border-dashed border-line-1
              px-2 py-0.5 text-reported text-ink-3
              hover:border-warm hover:text-warm hover:bg-warm-2/40
              transition-colors
            "
          >
            <Plus size={10} />
            Add repo
          </button>
        </Popover.Trigger>
        <Popover.Portal>
          <Popover.Content
            sideOffset={6}
            align="start"
            className="
              z-50 w-72 rounded-xl border border-line-1
              bg-raised shadow-float shadow-black/[0.08] p-2
            "
          >
            <input
              type="text"
              autoFocus
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              placeholder="Search configured repos…"
              className="
                w-full rounded-lg border border-line-1
                bg-raised px-2.5 py-1.5 text-ui text-ink-1
                placeholder:text-ink-3 mb-1.5
                focus:outline-none focus:border-warm
              "
            />
            <div className="max-h-60 overflow-y-auto">
              {loading ? (
                <div className="text-ui text-ink-3 px-2 py-1">Loading…</div>
              ) : loadError ? (
                <div className="text-ui text-ink-3 px-2 py-1">
                  Couldn&rsquo;t load configured repos.{' '}
                  <button
                    type="button"
                    onClick={() => {
                      setLoading(true)
                      const controller = new AbortController()
                      loadRepos(controller.signal)
                    }}
                    className="text-warm hover:underline"
                  >
                    Try again
                  </button>
                  .
                </div>
              ) : available.length === 0 ? (
                <div className="text-ui text-ink-3 px-2 py-1">
                  No repos configured.{' '}
                  <Link to={orgHref('/repos')} className="text-warm hover:underline">
                    Add some
                  </Link>
                  .
                </div>
              ) : addable.length === 0 ? (
                <div className="text-ui text-ink-3 px-2 py-1 italic">
                  {pinned.length === available.length
                    ? 'All configured repos are pinned.'
                    : 'No matches.'}
                </div>
              ) : (
                addable.map((r) => (
                  <button
                    key={r.id}
                    type="button"
                    onClick={() => add(r.id)}
                    className="
                      w-full text-left px-2 py-1.5 rounded-md
                      text-ui text-ink-1
                      hover:bg-tint-3 transition-colors
                    "
                  >
                    {r.slug}
                  </button>
                ))
              )}
            </div>
          </Popover.Content>
        </Popover.Portal>
      </Popover.Root>
    </div>
  )
}

function RepoChip({ slug, onRemove }: { slug: string; onRemove: () => void }) {
  return (
    <span
      className="
        group inline-flex items-center rounded-full
        bg-tint-2 text-ink-2 border border-line-1
        pl-2 pr-1 py-0.5 text-reported
        hover:border-alarm/40 hover:bg-alarm/[0.04] transition-colors
      "
    >
      {slug}
      <button
        type="button"
        onClick={(e) => {
          e.stopPropagation()
          onRemove()
        }}
        aria-label={`Remove ${slug}`}
        className="
          ml-1 inline-flex items-center justify-center
          h-3.5 w-3.5 rounded-full
          opacity-0 group-hover:opacity-100 focus-visible:opacity-100
          text-ink-3 hover:text-alarm hover:bg-alarm/10
          focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-alarm/40
          transition-[opacity,color]
        "
      >
        <X size={10} />
      </button>
    </span>
  )
}

// VISIBILITY_RANK orders the three visibility values so a change can be
// classified as an upgrade (more readers) or a downgrade (fewer) — the
// downgrade case gets a confirm() warning since it can revoke access
// people currently have, mirroring the delete confirm() pattern already
// used on this page.
const VISIBILITY_RANK: Record<ProjectVisibility, number> = { private: 0, team: 1, org: 2 }

const VISIBILITY_LABEL: Record<ProjectVisibility, string> = {
  private: 'private (only you)',
  team: 'team (your team only)',
  org: 'org-wide',
}

// VisibilityPanel (TFAC-562) is multi-mode only — local's N=1 tenancy has
// no team/org distinction, so this renders nothing there and every local
// project stays implicitly "team" as it always has. Options are grayed
// out via the same rule the backend RLS enforces: "team" needs the
// project to already have one AND the viewer to be able to write it (v1
// doesn't support attaching a team to a project that has none — see
// handleProjectUpdate's 400 for that case); "org" needs an org admin;
// "private" needs the viewer to be the project's own creator. A downgrade
// (fewer future readers) prompts a confirm() first since it can revoke
// access people currently have; an upgrade applies immediately.
function VisibilityPanel({
  project,
  onPatch,
}: {
  project: Project
  onPatch: (body: Record<string, unknown>) => Promise<boolean | undefined>
}) {
  const { config } = useDeploymentConfig()
  const { isAdmin: orgIsAdmin } = useOrgRole()
  const { canWriteTeam } = useTeamRole()
  const { me } = useMe()
  const [pending, setPending] = useState(false)

  if (config?.deployment_mode !== 'multi') return null

  const canTeam = project.team_id !== '' && canWriteTeam(project.team_id)
  const canOrg = orgIsAdmin
  const canPrivate = !!me && me.id === project.creator_user_id

  const handleChange = async (next: ProjectVisibility) => {
    if (next === project.visibility || pending) return
    if (VISIBILITY_RANK[next] < VISIBILITY_RANK[project.visibility]) {
      const ok = confirm(
        `Change visibility to ${VISIBILITY_LABEL[next]}? People who can currently see this project may lose access.`,
      )
      if (!ok) return
    }
    setPending(true)
    try {
      await onPatch({ visibility: next })
    } finally {
      setPending(false)
    }
  }

  return (
    <Card>
      <h2 className="text-body font-semibold tracking-tight text-ink-1 uppercase mb-4">
        Visibility
      </h2>
      <ProjectVisibilitySelect
        value={project.visibility}
        onChange={handleChange}
        canTeam={canTeam}
        canOrg={canOrg}
        canPrivate={canPrivate}
      />
    </Card>
  )
}

// IntegrationsPanel is now just the tracker-projects section. Pinned
// repos live in the header. Auto-saves: each picker change triggers
// an immediate PATCH; the upstream project state is the source of
// truth and the panel re-renders from it on success.
function IntegrationsPanel({
  project,
  onPatch,
}: {
  project: Project
  onPatch: (body: Record<string, unknown>) => Promise<boolean | undefined>
}) {
  // Coalesce overlapping changes per side. The earlier "skip if
  // inflight" approach silently dropped the user's later selection
  // — fast switches (Jira: SKY → OPS → INFRA) would land on SKY and
  // ignore the rest. The queue-latest pattern guarantees the final
  // intent reaches the server while still serializing the writes
  // for each side independently.
  //
  // Per-side queues (jira vs. linear) so a slow Jira PATCH doesn't
  // block an unrelated Linear edit; their server-side validation
  // paths don't share state.
  const jiraInflight = useRef(false)
  const jiraTarget = useRef<string | null>(null)
  const linearInflight = useRef(false)
  const linearTarget = useRef<string | null>(null)

  const drainJira = async () => {
    while (jiraTarget.current !== null) {
      const target = jiraTarget.current
      jiraTarget.current = null
      const ok = await onPatch({ jira_project_key: target })
      if (!ok) {
        jiraTarget.current = null
        break
      }
    }
  }

  const drainLinear = async () => {
    while (linearTarget.current !== null) {
      const target = linearTarget.current
      linearTarget.current = null
      const ok = await onPatch({ linear_project_key: target })
      if (!ok) {
        linearTarget.current = null
        break
      }
    }
  }

  const handleJiraChange = async (key: string) => {
    jiraTarget.current = key
    if (jiraInflight.current) return
    jiraInflight.current = true
    try {
      await drainJira()
    } finally {
      jiraInflight.current = false
    }
  }

  const handleLinearChange = async (key: string) => {
    linearTarget.current = key
    if (linearInflight.current) return
    linearInflight.current = true
    try {
      await drainLinear()
    } finally {
      linearInflight.current = false
    }
  }

  return (
    <Card>
      <h2 className="text-body font-semibold tracking-tight text-ink-1 uppercase mb-4">
        Integrations
      </h2>
      <TrackerProjectPickers
        jiraKey={project.jira_project_key}
        linearKey={project.linear_project_key}
        onJiraChange={handleJiraChange}
        onLinearChange={handleLinearChange}
        teamId={project.team_id}
      />
    </Card>
  )
}

// KnowledgePanel is the read+write surface for the project's
// knowledge base. Two ways in: clicking "+ Add" opens the OS file
// picker, or dragging files from the desktop drops them onto the
// panel. Both paths funnel through uploadFiles which POSTs a
// multipart request and refreshes the listing.
//
// Render switch is mime-driven: markdown via react-markdown, images
// via <img> against the per-file raw endpoint, text-shaped types in
// a <pre>, anything else gets an "Open" link that opens the raw
// bytes in a new tab. The agent can read everything we store; the
// preview switch is purely for the user-facing panel.
function KnowledgePanel({ projectId }: { projectId: string }) {
  const [files, setFiles] = useState<KnowledgeFile[]>([])
  const [loading, setLoading] = useState(true)
  const [expanded, setExpanded] = useState<string | null>(null)
  const [uploading, setUploading] = useState(false)
  // syncing holds filenames the executor syncer reported as in-flight uploads
  // (multi mode's `pending` field). We render a ghost row per name not yet in
  // the durable listing, so a large upload reads as progress rather than a
  // confusing delay. Cleared when the batch-complete event (no pending) lands.
  const [syncing, setSyncing] = useState<string[]>([])
  // Synchronous ref so the drop/picker guards can reject overlapping
  // uploads without racing setState. A naive `if (uploading) return`
  // would let a second drop slip through between the first call's
  // `setUploading(true)` and the next render — and crucially, the
  // first call's `finally { setUploading(false) }` would re-enable
  // the UI while the second call is still running.
  const uploadInflight = useRef(0)
  const [dragOver, setDragOver] = useState(false)
  const fileInputRef = useRef<HTMLInputElement>(null)
  // Counter, not boolean: dragenter/dragleave fire for every nested
  // child element so a naive `setDragOver(true/false)` flickers off
  // when crossing inner DOM boundaries. The counter increments on
  // enter and decrements on leave; the visual state is "any drag in
  // progress" iff counter > 0.
  const dragDepth = useRef(0)
  // refreshSeq gates refresh responses to "the most recent fetch
  // currently in flight." Unlike PATCH responses (where each carries
  // post-mutation state and an older success is still authoritative
  // for that mutation), a GET reflects the filesystem at the time
  // of the read — older GETs are unconditionally stale relative to
  // any newer GET that started after them. So the check is the
  // straightforward "drop if I'm not the latest issued."
  //
  // The previous "land older success even after a newer failure"
  // logic introduced a worse bug: in the upload → refresh flow, a
  // newer refresh that errored would let an older pre-upload
  // refresh land afterward and repaint the listing without the
  // just-uploaded files. Better to keep stale-rendered data than to
  // actively replace fresh data with stale data.
  const refreshSeq = useRef(0)

  const refreshFiles = useCallback(async () => {
    refreshSeq.current += 1
    const mySeq = refreshSeq.current
    try {
      const data = await apiJSON<KnowledgeFile[]>(
        `/api/projects/${encodeURIComponent(projectId)}/knowledge`,
      )
      if (mySeq !== refreshSeq.current) return
      setFiles(data)
    } catch (err) {
      if (mySeq !== refreshSeq.current) return
      toast.error(httpErrorMessage(err, 'Could not load the knowledge base.'))
    }
  }, [projectId])

  useEffect(() => {
    let cancelled = false
    refreshFiles().finally(() => {
      if (!cancelled) setLoading(false)
    })
    return () => {
      cancelled = true
      // Bump the seq on unmount so any in-flight refresh that
      // resolves later short-circuits before touching state.
      refreshSeq.current += 1
    }
  }, [refreshFiles])

  // Live updates: the backend fires `project_knowledge_updated` whenever
  // the agent (or any other writer) touches a file under
  // <projectsRoot>/<id>/knowledge-base/. We refetch on receipt so files
  // appear in the panel as the agent writes them mid-turn. Filter on
  // project_id so other projects' knowledge edits don't trigger
  // refetches here.
  useWebSocket((event) => {
    if (event.type !== 'project_knowledge_updated') return
    if (event.project_id !== projectId) return
    // The executor syncer always carries a `pending` field — the batch's names
    // on start, an empty array on completion — so we drive ghost rows only off
    // those. The control pod's own upload/delete broadcasts `data: null` (no
    // pending field); it must NOT clear ghost rows for an unrelated in-flight
    // executor batch, so we leave `syncing` untouched and just refetch.
    const pending = event.data?.pending
    if (pending !== undefined) {
      setSyncing(pending)
    }
    refreshFiles()
  })

  const uploadFiles = useCallback(
    async (fileList: FileList | File[]) => {
      const arr = Array.from(fileList)
      if (arr.length === 0) return
      if (uploadInflight.current > 0) {
        toast.warning('Another upload is in progress — wait for it to finish.')
        return
      }
      uploadInflight.current += 1
      setUploading(true)
      try {
        const form = new FormData()
        for (const f of arr) form.append('file', f)
        const data = await apiJSON<{ results: KnowledgeUploadResult[] }>(
          `/api/projects/${encodeURIComponent(projectId)}/knowledge`,
          { method: 'POST', body: form },
        )
        const ok = data.results.filter((r) => !r.error)
        const failed = data.results.filter((r) => r.error)
        if (ok.length > 0) {
          toast.success(
            ok.length === 1
              ? `Added ${ok[0].path}`
              : `Added ${ok.length} files to the knowledge base`,
          )
        }
        for (const f of failed) {
          toast.error(`${f.original}: ${f.error}`)
        }
        await refreshFiles()
      } catch (err) {
        toast.error(httpErrorMessage(err, 'Could not upload the files.'))
      } finally {
        uploadInflight.current -= 1
        if (uploadInflight.current === 0) setUploading(false)
      }
    },
    [projectId, refreshFiles],
  )

  const handleDelete = useCallback(
    async (file: KnowledgeFile) => {
      if (!confirm(`Remove ${file.path} from the knowledge base?`)) return
      try {
        await apiFetch(
          `/api/projects/${encodeURIComponent(projectId)}/knowledge/${encodeURIComponent(file.path)}`,
          { method: 'DELETE' },
        )
        setFiles((prev) => prev.filter((f) => f.path !== file.path))
        if (expanded === file.path) setExpanded(null)
      } catch (err) {
        toast.error(httpErrorMessage(err, 'Could not remove the file.'))
      }
    },
    [projectId, expanded],
  )

  const handleDragEnter = (e: React.DragEvent) => {
    if (!hasFiles(e)) return
    e.preventDefault()
    dragDepth.current += 1
    setDragOver(true)
  }
  const handleDragLeave = (e: React.DragEvent) => {
    if (!hasFiles(e)) return
    e.preventDefault()
    dragDepth.current = Math.max(0, dragDepth.current - 1)
    if (dragDepth.current === 0) setDragOver(false)
  }
  const handleDragOver = (e: React.DragEvent) => {
    if (!hasFiles(e)) return
    e.preventDefault()
    e.dataTransfer.dropEffect = 'copy'
  }
  const handleDrop = (e: React.DragEvent) => {
    if (!hasFiles(e)) return
    e.preventDefault()
    dragDepth.current = 0
    setDragOver(false)
    const dropped = e.dataTransfer.files
    if (dropped && dropped.length > 0) {
      uploadFiles(dropped)
    }
  }

  // Ghost rows: syncing names not yet present in the durable listing. As each
  // upload lands and a refetch brings it into `files`, its ghost drops out.
  const knownPaths = new Set(files.map((f) => f.path))
  const ghostNames = syncing.filter((n) => !knownPaths.has(n))

  return (
    <Card
      className={`transition-shadow duration-200 ${dragOver ? 'ring-2 ring-warm' : ''}`}
      onDragEnter={handleDragEnter}
      onDragLeave={handleDragLeave}
      onDragOver={handleDragOver}
      onDrop={handleDrop}
    >
      <header className="flex items-center justify-between mb-4">
        <h2 className="text-body font-semibold tracking-tight text-ink-1 uppercase">
          Knowledge base
        </h2>
        <button
          type="button"
          onClick={() => fileInputRef.current?.click()}
          disabled={uploading}
          className="
            inline-flex items-center gap-1.5 rounded-full
            px-3 py-1 text-ui
            text-warm hover:bg-warm-2
            disabled:opacity-50 transition-colors
          "
        >
          <Plus size={12} />
          {uploading ? 'Uploading…' : 'Add'}
        </button>
        <input
          ref={fileInputRef}
          type="file"
          multiple
          className="hidden"
          onChange={(e) => {
            if (e.target.files) uploadFiles(e.target.files)
            // Reset so re-selecting the same file fires onChange again.
            e.target.value = ''
          }}
        />
      </header>

      {loading ? (
        <div className="text-ui text-ink-3">Loading…</div>
      ) : files.length === 0 && ghostNames.length === 0 ? (
        <div className="text-ui text-ink-3 italic py-4 text-center">
          No knowledge files yet. Drop files here or click <span className="not-italic">+ Add</span>
          .
        </div>
      ) : (
        // Cap the KB list so the entities panel below has
        // breathing room in the left column. Unbounded growth would
        // push the entities panel below the fold on a typical laptop.
        <div className="max-h-[50vh] overflow-y-auto space-y-2 pr-1">
          {files.map((file) => (
            <KnowledgeRow
              key={file.path}
              projectId={projectId}
              file={file}
              expanded={expanded === file.path}
              onToggle={() => setExpanded(expanded === file.path ? null : file.path)}
              onDelete={() => handleDelete(file)}
            />
          ))}
          {ghostNames.map((name) => (
            <KnowledgeSyncingRow key={`syncing:${name}`} name={name} />
          ))}
        </div>
      )}
    </Card>
  )
}

function ProjectExportModal({
  projectId,
  projectName,
  onClose,
}: {
  projectId: string
  projectName: string
  onClose: () => void
}) {
  const [preview, setPreview] = useState<ProjectExportPreview | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [exporting, setExporting] = useState(false)

  // Trap keyboard focus inside the dialog and restore it to the trigger on
  // close (WCAG 2.1.2).
  const dialogRef = useRef<HTMLDivElement>(null)
  useFocusTrap(dialogRef)

  useEffect(() => {
    let cancelled = false
    setLoading(true)
    setError(null)
    apiJSON<ProjectExportPreview>(`/api/projects/${encodeURIComponent(projectId)}/export/preview`)
      .then((data) => {
        if (!cancelled) setPreview(data)
      })
      .catch((err: unknown) => {
        if (!cancelled) {
          setError(httpErrorMessage(err, 'Could not load the export preview.'))
        }
      })
      .finally(() => {
        if (!cancelled) setLoading(false)
      })
    return () => {
      cancelled = true
    }
  }, [projectId])

  const startExport = async () => {
    setExporting(true)
    setError(null)
    try {
      const res = await apiFetch(`/api/projects/${encodeURIComponent(projectId)}/export`)
      const blob = await res.blob()
      const fallback = `${projectName || 'project'}.tfproject`
      const filename = extractFilename(res.headers.get('Content-Disposition')) || fallback
      const url = URL.createObjectURL(blob)
      const a = document.createElement('a')
      a.href = url
      a.download = filename
      document.body.appendChild(a)
      a.click()
      a.remove()
      URL.revokeObjectURL(url)
      toast.success(`Exported "${projectName}"`)
      onClose()
    } catch (err) {
      setError(httpErrorMessage(err, 'Could not export the project.'))
    } finally {
      setExporting(false)
    }
  }

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-scrim backdrop-blur-sm"
      onClick={() => {
        if (!exporting) onClose()
      }}
    >
      <div
        className="
          relative w-full max-w-2xl
          rounded-2xl border border-line-1
          bg-raised
          shadow-float shadow-black/[0.08] backdrop-blur-xl
          p-6
        "
        ref={dialogRef}
        tabIndex={-1}
        role="dialog"
        aria-modal="true"
        aria-labelledby="project-export-title"
        onClick={(e) => e.stopPropagation()}
      >
        <h2
          id="project-export-title"
          className="text-lg font-semibold tracking-tight text-ink-1 mb-1"
        >
          Review export contents
        </h2>
        <p className="text-ui text-ink-3 mb-4">
          This bundle includes everything listed below. Review before sharing.
        </p>

        {loading ? (
          <div className="text-ui text-ink-3">Loading preview…</div>
        ) : error ? (
          <div className="rounded-lg border border-alarm/20 bg-alarm/5 px-3 py-2 text-ui text-alarm">
            {error}
          </div>
        ) : (
          <div className="space-y-3">
            <div className="max-h-72 overflow-y-auto rounded-lg border border-line-1 bg-raised">
              {(preview?.files || []).map((file) => (
                <div
                  key={file.path}
                  className="flex items-center justify-between gap-3 border-b last:border-b-0 border-line-1 px-3 py-2"
                >
                  <span className="text-ui text-ink-1 truncate">{file.path}</span>
                  <span className="text-reported text-ink-3 tabular-nums shrink-0">
                    {formatBytes(file.size_bytes)}
                  </span>
                </div>
              ))}
              {preview && preview.files.length === 0 && (
                <div className="px-3 py-2 text-ui text-ink-3 italic">No files to export.</div>
              )}
            </div>
            <div className="text-ui text-ink-2">
              Total size:{' '}
              <span className="font-medium text-ink-1">
                {formatBytes(preview?.total_size || 0)}
              </span>
            </div>
            {(preview?.warnings || []).map((warning) => (
              <div
                key={warning}
                className="rounded-lg border border-warm/25 bg-warm/5 px-3 py-2 text-ui text-warm"
              >
                {warning}
              </div>
            ))}
          </div>
        )}

        <div className="flex justify-end gap-2 pt-5">
          <button
            type="button"
            onClick={onClose}
            disabled={exporting}
            className="
              rounded-full px-4 py-2 text-body
              text-ink-2 hover:text-ink-1 hover:bg-tint-2
              transition-all disabled:opacity-50
            "
          >
            Cancel
          </button>
          <button
            type="button"
            onClick={startExport}
            disabled={loading || exporting || !!error}
            className="
              rounded-full px-4 py-2 text-body font-medium
              bg-warm text-warm-ink hover:opacity-90
              disabled:opacity-50 transition-all
            "
          >
            {exporting ? 'Exporting…' : 'Download .tfproject'}
          </button>
        </div>
      </div>
    </div>
  )
}

function extractFilename(contentDisposition: string | null): string | null {
  if (!contentDisposition) return null
  const match = /filename="?([^"]+)"?/i.exec(contentDisposition)
  if (!match || !match[1]) return null
  return match[1]
}

// hasFiles guards drag handlers against drag operations that aren't
// carrying files (e.g. text/url drags from other parts of the app or
// tabs). Without it the panel would highlight on every dragover from
// a chip drag elsewhere on the page.
function hasFiles(e: React.DragEvent): boolean {
  const types = e.dataTransfer?.types
  if (!types) return false
  for (let i = 0; i < types.length; i++) {
    if (types[i] === 'Files') return true
  }
  return false
}

// KnowledgeRow renders a single file. Expand toggles the inline
// preview, which the row chooses based on mime_type:
//   - text/markdown → react-markdown
//   - image/* → <img> from the raw endpoint
//   - other text-shaped → <pre>
//   - anything else → "Open in new tab" link to raw endpoint
//
// Empty content with a text-shaped mime means the file was over the
// inline-size limit; we lazy-fetch via the raw endpoint on first
// expand.
// KnowledgeSyncingRow is the ghost row shown for a file the executor syncer is
// still uploading to the blob store (multi mode's `pending` names). It mirrors
// KnowledgeRow's frame with a spinner and no actions, so a large upload reads
// as in-progress until its durable listing entry arrives and replaces it.
function KnowledgeSyncingRow({ name }: { name: string }) {
  return (
    <div className="rounded-lg border border-line-1 border-dashed bg-raised overflow-hidden">
      <div className="flex items-center gap-3 px-3 py-2 min-w-0">
        <Loader2 size={14} className="shrink-0 animate-spin text-ink-3" />
        <span className="flex-1 truncate text-body text-ink-3">{name}</span>
        <span className="shrink-0 text-reported text-ink-3 italic">Syncing…</span>
      </div>
    </div>
  )
}

function KnowledgeRow({
  projectId,
  file,
  expanded,
  onToggle,
  onDelete,
}: {
  projectId: string
  file: KnowledgeFile
  expanded: boolean
  onToggle: () => void
  onDelete: () => void
}) {
  const rawURL = `/api/projects/${encodeURIComponent(projectId)}/knowledge/${encodeURIComponent(file.path)}`
  const isMarkdown = file.mime_type.startsWith('text/markdown')
  const isImage = file.mime_type.startsWith('image/')
  const isText = isTextMime(file.mime_type)

  // Tri-state: null = not fetched yet, string (incl. "") = fetched.
  // The loading flag is derived rather than stored — sidesteps the
  // react-hooks/set-state-in-effect lint that flags a synchronous
  // setLazyLoading(true) inside the effect body.
  const [lazyContent, setLazyContent] = useState<string | null>(null)
  const needsLazyFetch = expanded && isText && !file.content && lazyContent === null

  useEffect(() => {
    if (!needsLazyFetch) return
    let cancelled = false
    // The knowledge endpoint serves the file's own bytes, not JSON — apiFetch
    // and .text(), not apiJSON.
    apiFetch(rawURL)
      .then((r) => r.text())
      .then((text) => {
        if (!cancelled) setLazyContent(text)
      })
      .catch((error: unknown) => {
        if (cancelled) return
        toast.error(httpErrorMessage(error, 'Could not load the file preview.'))
        setLazyContent('Failed to load file preview.')
      })
    return () => {
      cancelled = true
    }
  }, [file.path, needsLazyFetch, rawURL])

  const lazyLoading = needsLazyFetch

  const Icon = isImage ? ImageIcon : isText ? FileText : FileIcon

  return (
    <div className="group rounded-lg border border-line-1 bg-raised overflow-hidden">
      <div className="flex items-center gap-2 pr-2">
        <button
          type="button"
          onClick={onToggle}
          className="
            flex-1 flex items-center justify-between gap-3
            px-3 py-2 text-left min-w-0
            hover:bg-tint-2 transition-colors
          "
        >
          <span className="flex items-center gap-2 min-w-0">
            <Icon size={12} className="text-ink-3 shrink-0" />
            <span className="text-ui font-medium text-ink-1 truncate">{file.path}</span>
          </span>
          <span className="text-label text-ink-3 tabular-nums shrink-0">
            {formatBytes(file.size_bytes)}
          </span>
        </button>
        <button
          type="button"
          onClick={onDelete}
          aria-label={`Remove ${file.path}`}
          className="
            inline-flex items-center justify-center h-6 w-6 rounded-full
            opacity-0 group-hover:opacity-100 focus-visible:opacity-100
            text-ink-3 hover:text-alarm hover:bg-alarm/[0.08]
            focus:outline-none focus-visible:ring-2 focus-visible:ring-alarm
            transition-[opacity,color,background-color]
          "
        >
          <X size={12} />
        </button>
      </div>
      {expanded && (
        <div className="border-t border-line-1 px-4 py-3">
          {isMarkdown ? (
            <div className="prose prose-sm max-w-none text-ui text-ink-2 leading-relaxed">
              <Markdown>{file.content || lazyContent || ''}</Markdown>
            </div>
          ) : isImage ? (
            <img
              src={rawURL}
              alt={file.path}
              className="max-w-full max-h-96 rounded-md mx-auto block"
            />
          ) : isText ? (
            lazyLoading ? (
              <div className="text-ui text-ink-3">Loading…</div>
            ) : (
              <pre className="text-reported text-ink-2 leading-relaxed whitespace-pre-wrap break-words font-mono max-h-96 overflow-auto">
                {file.content || lazyContent || ''}
              </pre>
            )
          ) : (
            <div className="flex items-center justify-between gap-3">
              <span className="text-ui text-ink-3 italic">
                No inline preview for {file.mime_type || 'this file type'}.
              </span>
              <a
                href={rawURL}
                target="_blank"
                rel="noreferrer"
                className="
                  inline-flex items-center gap-1 rounded-full
                  bg-warm-2 text-warm px-3 py-1 text-reported
                  hover:opacity-90
                "
              >
                <ExternalLink size={10} />
                Open
              </a>
            </div>
          )}
        </div>
      )}
    </div>
  )
}

// isTextMime mirrors the backend's classification so the frontend
// renders the same set as a <pre>. Source-of-truth lives in the
// listing's MimeType for the binary/text branch on which content is
// inlined; this just controls how the row renders.
function isTextMime(mimeType: string): boolean {
  if (!mimeType) return false
  const main = mimeType.split(';')[0].trim()
  if (main.startsWith('text/')) return true
  return [
    'application/json',
    'application/yaml',
    'application/x-yaml',
    'application/xml',
    'application/javascript',
    'application/typescript',
    'application/toml',
  ].includes(main)
}

// Card spreads through any HTML section attributes so callers can
// attach drag handlers, aria-* attrs, etc., without forcing a custom
// prop list. KnowledgePanel uses this to wire onDragEnter/onDrop/etc.
// onto the panel chrome for the drag-and-drop upload path.
function Card({
  children,
  className = '',
  ...rest
}: {
  children: React.ReactNode
  className?: string
} & Omit<React.HTMLAttributes<HTMLElement>, 'className' | 'children'>) {
  return (
    <section
      className={`
        relative overflow-hidden rounded-2xl border border-line-1
        bg-raised
        p-5 shadow-float shadow-black/[0.03] backdrop-blur-xl
        ${className}
      `}
      {...rest}
    >
      <span
        aria-hidden
        className="pointer-events-none absolute -left-8 -top-8 h-24 w-24 rounded-full bg-raised blur-2xl"
      />
      <div className="relative">{children}</div>
    </section>
  )
}

function Chip({ label, tone }: { label: string; tone: 'accent' | 'muted' }) {
  const cls =
    tone === 'accent' ? 'bg-warm-2 text-warm' : 'bg-tint-2 text-ink-2 border border-line-1'
  return (
    <span className={`inline-flex items-center rounded-full px-2 py-0.5 text-reported ${cls}`}>
      {label}
    </span>
  )
}

function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`
  return `${(n / 1024 / 1024).toFixed(2)} MB`
}
