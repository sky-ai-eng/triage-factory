import { useState, useEffect, useCallback, useRef } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { Plus, Trash2, Upload } from 'lucide-react'
import { useOrgHref } from '../hooks/useOrgHref'
import type { Project, ProjectImportError, ProjectImportResult } from '../types'
import { readError } from '../lib/api'
import { useFocusTrap } from '../hooks/useFocusTrap'
import { toast } from '../components/Toast/toastStore'
import ProjectCreateModal from '../components/ProjectCreateModal'
import ProjectBackfillModal from '../components/ProjectBackfillModal'

// Projects index. List view only — the per-project view lives in
// ProjectDetail.tsx and the Curator chat panel will graft into it.
// We keep the visual language tight enough that a project
// with zero pinned repos / no tracker / no description still renders
// as a recognizable card rather than collapsing into nothing.
//
// Empty-state contract: zero projects renders a centered
// "Create your first project" CTA, not an empty grid. The full grid
// only appears once at least one project exists.
export default function Projects() {
  const navigate = useNavigate()
  const orgHref = useOrgHref()
  const [projects, setProjects] = useState<Project[]>([])
  const [loading, setLoading] = useState(true)
  const [createOpen, setCreateOpen] = useState(false)
  const [importOpen, setImportOpen] = useState(false)
  const [importSeedFile, setImportSeedFile] = useState<File | null>(null)
  // After a project is created OR imported, surface a
  // popup that lets the user reclaim existing non-terminal entities
  // into the new project. `backfillTarget` carries the destination;
  // `backfillThenNavigate` records whether the popup's close handler
  // should also navigate to the project page (true on import, false
  // on create — matching each flow's existing post-success behavior).
  const [backfillTarget, setBackfillTarget] = useState<Project | null>(null)
  const [backfillThenNavigate, setBackfillThenNavigate] = useState(false)
  const [pageDragOver, setPageDragOver] = useState(false)
  const pageDragDepth = useRef(0)
  // Distinguish "load failed" from "loaded but empty" so a transient
  // network error doesn't render the "Create your first project"
  // empty state — that would silently lie about the user's data.
  const [loadError, setLoadError] = useState<string | null>(null)

  const refresh = useCallback(async () => {
    try {
      setLoadError(null)
      const res = await fetch('/api/projects')
      if (!res.ok) {
        const msg = await readError(res, 'Failed to load projects')
        setLoadError(msg)
        toast.error(msg)
        return
      }
      const data: Project[] = await res.json()
      setProjects(data)
    } catch (err) {
      const msg = `Failed to load projects: ${err instanceof Error ? err.message : String(err)}`
      setLoadError(msg)
      toast.error(msg)
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    refresh()
  }, [refresh])

  const handleCreated = useCallback(
    (created: Project) => {
      setCreateOpen(false)
      setProjects((prev) => [...prev, created].sort((a, b) => a.name.localeCompare(b.name)))
      // Re-fetch to pick up server-generated fields we don't model
      // optimistically (e.g. anything the server post-processes).
      refresh()
      // Surface the backfill popup. Stay on the grid after — matches
      // the prior create flow that didn't navigate either.
      setBackfillTarget(created)
      setBackfillThenNavigate(false)
    },
    [refresh],
  )

  const handleImported = useCallback((created: Project) => {
    setImportOpen(false)
    setImportSeedFile(null)
    setProjects((prev) => [...prev, created].sort((a, b) => a.name.localeCompare(b.name)))
    // Defer the /projects/:id navigation until the backfill popup
    // closes — the user expects to land on the imported project,
    // but the popup is the chance to claim existing entities first.
    setBackfillTarget(created)
    setBackfillThenNavigate(true)
  }, [])

  const handleBackfillClose = useCallback(() => {
    const target = backfillTarget
    const navigateAfter = backfillThenNavigate
    setBackfillTarget(null)
    setBackfillThenNavigate(false)
    if (navigateAfter && target) {
      navigate(orgHref(`/projects/${encodeURIComponent(target.id)}`))
    }
  }, [backfillTarget, backfillThenNavigate, navigate, orgHref])

  const closeImportModal = useCallback(() => {
    setImportOpen(false)
    setImportSeedFile(null)
  }, [])

  const clearPageDrag = () => {
    pageDragDepth.current = 0
    setPageDragOver(false)
  }

  const handlePageDragEnter = (e: React.DragEvent<HTMLDivElement>) => {
    if (createOpen || importOpen || !hasDroppedFiles(e.dataTransfer)) return
    e.preventDefault()
    pageDragDepth.current += 1
    setPageDragOver(true)
  }

  const handlePageDragLeave = (e: React.DragEvent<HTMLDivElement>) => {
    if (createOpen || importOpen || !hasDroppedFiles(e.dataTransfer)) return
    e.preventDefault()
    pageDragDepth.current = Math.max(0, pageDragDepth.current - 1)
    if (pageDragDepth.current === 0) setPageDragOver(false)
  }

  const handlePageDragOver = (e: React.DragEvent<HTMLDivElement>) => {
    if (createOpen || importOpen || !hasDroppedFiles(e.dataTransfer)) return
    e.preventDefault()
    e.dataTransfer.dropEffect = 'copy'
  }

  const handlePageDrop = (e: React.DragEvent<HTMLDivElement>) => {
    if (createOpen || importOpen || !hasDroppedFiles(e.dataTransfer)) return
    e.preventDefault()
    clearPageDrag()
    const dropped = pickTfprojectFile(e.dataTransfer.files)
    if (!dropped) {
      toast.error('Only .tfproject files can be imported.')
      return
    }
    setImportSeedFile(dropped)
    setImportOpen(true)
  }

  // handleDelete mirrors the confirm-then-DELETE flow in ProjectDetail
  // so the grid-hover trash icon and the in-detail Delete button share
  // identical semantics. We optimistically drop the row from local
  // state on success rather than re-fetching the list — `refresh`'s
  // round-trip would also be fine, but the optimistic path keeps the
  // grid from briefly showing a stale entry.
  const handleDelete = useCallback(async (project: Project) => {
    if (!confirm(`Delete project "${project.name}"? This can't be undone.`)) return
    try {
      const res = await fetch(`/api/projects/${encodeURIComponent(project.id)}`, {
        method: 'DELETE',
      })
      if (!res.ok && res.status !== 204) {
        toast.error(await readError(res, 'Failed to delete project'))
        return
      }
      const cleanupWarning = res.headers.get('X-Cleanup-Warning')
      if (cleanupWarning) {
        toast.warning(cleanupWarning)
      } else {
        toast.success(`Deleted project "${project.name}"`)
      }
      setProjects((prev) => prev.filter((p) => p.id !== project.id))
    } catch (err) {
      toast.error(`Failed to delete project: ${err instanceof Error ? err.message : String(err)}`)
    }
  }, [])

  const content = loading ? (
    <div className="text-text-tertiary text-[13px]">Loading projects…</div>
  ) : loadError ? (
    <ErrorState message={loadError} onRetry={refresh} />
  ) : projects.length === 0 ? (
    <EmptyState onCreate={() => setCreateOpen(true)} />
  ) : (
    <div className="grid grid-cols-1 md:grid-cols-2 xl:grid-cols-3 gap-5">
      {projects.map((p) => (
        <ProjectCard key={p.id} project={p} onDelete={() => handleDelete(p)} />
      ))}
    </div>
  )

  return (
    <div
      className="max-w-6xl mx-auto h-[calc(100dvh-10rem)] min-h-[24rem] flex flex-col"
      onDragEnter={handlePageDragEnter}
      onDragLeave={handlePageDragLeave}
      onDragOver={handlePageDragOver}
      onDrop={handlePageDrop}
    >
      <header className="flex items-center justify-between mb-8 shrink-0">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight text-text-primary">Projects</h1>
          <p className="text-[13px] text-text-secondary mt-1">
            Group work by concept. Pin repos and tracker projects for the Curator to reason about.
          </p>
        </div>
        <div className="flex items-center gap-2">
          <button
            type="button"
            onClick={() => {
              setImportSeedFile(null)
              setImportOpen(true)
            }}
            className="
              inline-flex items-center gap-2 rounded-full
              border border-border-subtle bg-white/60
              text-[13px] text-text-secondary font-medium
              px-4 py-2 transition-all
              hover:bg-white hover:text-text-primary
            "
          >
            <Upload size={14} />
            Import
          </button>
          {projects.length > 0 && (
            <button
              type="button"
              onClick={() => setCreateOpen(true)}
              className="
                inline-flex items-center gap-2 rounded-full
                bg-accent text-white text-[13px] font-medium
                px-4 py-2 transition-all
                hover:opacity-90
              "
            >
              <Plus size={14} />
              New project
            </button>
          )}
        </div>
      </header>

      <div className="min-h-0 flex-1 overflow-y-auto pr-1">{content}</div>

      {pageDragOver && (
        <div className="fixed inset-0 z-40 pointer-events-none flex items-center justify-center bg-black/10">
          <div className="rounded-xl border border-accent bg-accent-soft px-6 py-3 text-[13px] font-medium text-accent shadow-lg">
            Drop .tfproject to import project
          </div>
        </div>
      )}

      {createOpen && (
        <ProjectCreateModal onClose={() => setCreateOpen(false)} onCreated={handleCreated} />
      )}
      {importOpen && (
        <ProjectImportModal
          onClose={closeImportModal}
          onImported={handleImported}
          initialFile={importSeedFile}
        />
      )}
      {backfillTarget && (
        <ProjectBackfillModal
          projectId={backfillTarget.id}
          projectName={backfillTarget.name}
          onClose={handleBackfillClose}
        />
      )}
    </div>
  )
}

function ErrorState({ message, onRetry }: { message: string; onRetry: () => void }) {
  return (
    <div className="flex flex-col items-center justify-center py-24">
      <div className="text-text-secondary text-[13px] max-w-md text-center mb-6">{message}</div>
      <button
        type="button"
        onClick={onRetry}
        className="
          inline-flex items-center gap-2 rounded-full
          bg-accent text-white text-[13px] font-medium
          px-5 py-2.5 transition-all
          hover:opacity-90
        "
      >
        Try again
      </button>
    </div>
  )
}

function EmptyState({ onCreate }: { onCreate: () => void }) {
  return (
    <div className="flex flex-col items-center justify-center py-24">
      <div className="text-text-tertiary text-[13px] max-w-md text-center mb-6">
        Projects bundle pinned repos, a Jira/Linear project, and a knowledge base — the Curator
        works inside that scope when you chat with it.
      </div>
      <button
        type="button"
        onClick={onCreate}
        className="
          inline-flex items-center gap-2 rounded-full
          bg-accent text-white text-[13px] font-medium
          px-5 py-2.5 transition-all
          hover:opacity-90
        "
      >
        <Plus size={14} />
        Create your first project
      </button>
    </div>
  )
}

function ProjectImportModal({
  onClose,
  onImported,
  initialFile,
}: {
  onClose: () => void
  onImported: (project: Project) => void
  initialFile?: File | null
}) {
  const [file, setFile] = useState<File | null>(initialFile ?? null)
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<ProjectImportError | null>(null)
  const [dragOver, setDragOver] = useState(false)
  const fileInputRef = useRef<HTMLInputElement | null>(null)
  const dragDepth = useRef(0)

  // Trap keyboard focus inside the dialog and restore it to the trigger on
  // close (WCAG 2.1.2). Initial focus lands on "Choose file" — the sr-only
  // file input that precedes it in the DOM would otherwise take focus with
  // no visible indicator.
  const dialogRef = useRef<HTMLDivElement>(null)
  const chooseFileRef = useRef<HTMLButtonElement>(null)
  useFocusTrap(dialogRef, { initialFocus: chooseFileRef })

  const clearSelectedFile = () => {
    setFile(null)
    if (fileInputRef.current) {
      fileInputRef.current.value = ''
    }
  }

  const selectBundle = (next: File | null) => {
    if (!next) {
      clearSelectedFile()
      return
    }
    if (!isTfprojectFile(next)) {
      clearSelectedFile()
      setError({
        error: 'invalid_file',
        message: 'Only .tfproject files can be imported.',
      })
      return
    }
    setFile(next)
    setError(null)
  }

  useEffect(() => {
    setFile(initialFile ?? null)
    setError(null)
  }, [initialFile])

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!file) {
      setError({ error: 'no_file', message: 'Choose a .tfproject file to import.' })
      return
    }
    setSubmitting(true)
    setError(null)
    try {
      const form = new FormData()
      form.append('bundle', file)
      const res = await fetch('/api/projects/import', { method: 'POST', body: form })
      if (!res.ok) {
        let parsed: ProjectImportError | null = null
        try {
          parsed = (await res.clone().json()) as ProjectImportError
        } catch {
          parsed = null
        }
        if (parsed) {
          setError(parsed)
        } else {
          setError({ error: 'import_failed', message: await readError(res, 'Import failed') })
        }
        return
      }
      const result = (await res.json()) as ProjectImportResult
      for (const warning of result.warnings || []) {
        toast.warning(
          warning.repo
            ? `${warning.repo}: ${warning.message}`
            : `Import warning: ${warning.message}`,
        )
      }
      toast.success(`Imported project "${result.project.name}"`)
      onImported(result.project)
    } catch (err) {
      setError({
        error: 'import_failed',
        message: `Import failed: ${err instanceof Error ? err.message : String(err)}`,
      })
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/30 backdrop-blur-sm"
      onClick={() => {
        if (!submitting) onClose()
      }}
    >
      <div
        className="
          relative w-full max-w-lg
          rounded-2xl border border-border-glass
          bg-gradient-to-br from-white/95 via-white/90 to-white/85
          shadow-xl shadow-black/[0.08] backdrop-blur-xl
          p-6
        "
        ref={dialogRef}
        tabIndex={-1}
        role="dialog"
        aria-modal="true"
        aria-labelledby="project-import-title"
        onClick={(e) => e.stopPropagation()}
      >
        <h2
          id="project-import-title"
          className="text-lg font-semibold tracking-tight text-text-primary mb-1"
        >
          Import project
        </h2>
        <p className="text-[12px] text-text-tertiary mb-4">
          Choose a <code>.tfproject</code> bundle exported from another machine.
        </p>

        <form onSubmit={submit} className="space-y-4">
          <label
            htmlFor="project-import-file"
            className="block text-[12px] font-medium text-text-secondary mb-1.5"
          >
            Bundle file
          </label>
          <input
            id="project-import-file"
            ref={fileInputRef}
            type="file"
            accept=".tfproject,application/zip"
            disabled={submitting}
            onChange={(e) => selectBundle(e.target.files?.[0] ?? null)}
            className="sr-only"
          />
          <div
            onDragEnter={(e) => {
              if (submitting || !hasDroppedFiles(e.dataTransfer)) return
              e.preventDefault()
              dragDepth.current += 1
              setDragOver(true)
            }}
            onDragLeave={(e) => {
              if (submitting || !hasDroppedFiles(e.dataTransfer)) return
              e.preventDefault()
              dragDepth.current = Math.max(0, dragDepth.current - 1)
              if (dragDepth.current === 0) setDragOver(false)
            }}
            onDragOver={(e) => {
              if (submitting || !hasDroppedFiles(e.dataTransfer)) return
              e.preventDefault()
              e.dataTransfer.dropEffect = 'copy'
            }}
            onDrop={(e) => {
              if (submitting || !hasDroppedFiles(e.dataTransfer)) return
              e.preventDefault()
              dragDepth.current = 0
              setDragOver(false)
              const picked = pickTfprojectFile(e.dataTransfer.files)
              if (!picked) {
                clearSelectedFile()
                setError({
                  error: 'invalid_file',
                  message: 'Only .tfproject files can be imported.',
                })
                return
              }
              selectBundle(picked)
            }}
            className={`
              w-full rounded-lg border border-dashed px-3 py-2
              flex items-center gap-3
              transition-colors
              ${
                dragOver
                  ? 'border-accent bg-accent-soft/50 ring-2 ring-accent/20'
                  : 'border-border-subtle bg-white/60'
              }
            `}
          >
            <button
              ref={chooseFileRef}
              type="button"
              onClick={() => fileInputRef.current?.click()}
              disabled={submitting}
              className="
                inline-flex items-center rounded-md
                border border-border-subtle bg-white
                px-3 py-1.5 text-[12px] font-medium text-text-secondary
                hover:text-text-primary hover:bg-white/90
                disabled:opacity-50
              "
            >
              {dragOver ? 'Drop file' : 'Choose file'}
            </button>
            <span
              className={`
                min-w-0 flex-1 truncate text-right text-[13px]
                ${dragOver && !file ? 'text-accent' : file ? 'text-text-primary' : 'text-text-tertiary'}
              `}
              title={file?.name}
            >
              {dragOver && !file ? 'Drop .tfproject here' : file?.name || 'No file chosen'}
            </span>
          </div>

          {error && (
            <div className="rounded-lg border border-dismiss/20 bg-dismiss/5 px-3 py-2 text-[12px] text-dismiss">
              {renderImportError(error)}
            </div>
          )}

          <div className="flex justify-end gap-2 pt-2">
            <button
              type="button"
              onClick={onClose}
              disabled={submitting}
              className="
                rounded-full px-4 py-2 text-[13px]
                text-text-secondary hover:text-text-primary hover:bg-black/[0.03]
                transition-all disabled:opacity-50
              "
            >
              Cancel
            </button>
            <button
              type="submit"
              disabled={submitting || !file}
              className="
                rounded-full px-4 py-2 text-[13px] font-medium
                bg-accent text-white hover:opacity-90
                disabled:opacity-50 transition-all
              "
            >
              {submitting ? 'Importing…' : 'Import project'}
            </button>
          </div>
        </form>
      </div>
    </div>
  )
}

function renderImportError(err: ProjectImportError): string {
  if (err.error === 'duplicate_name') {
    return err.message || 'A project with that name already exists. Rename or delete it first.'
  }
  if (err.error === 'missing_repos' && err.missing_repos && err.missing_repos.length > 0) {
    const list = err.missing_repos.map((m) => `${m.repo} (${m.error})`).join(', ')
    return `Missing pinned repos: ${list}`
  }
  return err.message || err.error || 'Import failed.'
}

function hasDroppedFiles(dataTransfer: DataTransfer | null): boolean {
  if (!dataTransfer) return false
  if (dataTransfer.items && dataTransfer.items.length > 0) {
    for (let i = 0; i < dataTransfer.items.length; i++) {
      if (dataTransfer.items[i].kind === 'file') return true
    }
  }
  if (!dataTransfer.types) return false
  for (let i = 0; i < dataTransfer.types.length; i++) {
    const t = dataTransfer.types[i]
    if (t === 'Files' || t === 'public.file-url' || t === 'application/x-moz-file') {
      return true
    }
  }
  return false
}

function isTfprojectFile(file: File): boolean {
  return file.name.toLowerCase().endsWith('.tfproject')
}

function pickTfprojectFile(files: FileList | null): File | null {
  if (!files) return null
  for (let i = 0; i < files.length; i++) {
    if (isTfprojectFile(files[i])) return files[i]
  }
  return null
}

function ProjectCard({ project, onDelete }: { project: Project; onDelete: () => void }) {
  const orgHref = useOrgHref()
  const desc = (project.description || '').trim()
  // Stretched-link pattern: the outer <article> is the visual card,
  // a transparent <Link> overlay covers it for navigation, and the
  // trash <button> is a sibling at higher z. This avoids the
  // <a><button></a> nesting an earlier draft had — invalid HTML and
  // unreliable for screen readers / keyboard nav. Tab order here is
  // "card link → delete button," each focusable in its own right.
  return (
    <article
      className="
        group relative overflow-hidden rounded-2xl border border-border-glass
        bg-gradient-to-br from-white/70 via-white/50 to-white/35
        p-5 shadow-sm shadow-black/[0.03] backdrop-blur-xl
        transition-[box-shadow,border-color] duration-300
        hover:border-white/90 hover:shadow-md hover:shadow-black/[0.05]
      "
    >
      <span
        aria-hidden
        className="pointer-events-none absolute -left-8 -top-8 h-24 w-24 rounded-full bg-white/30 blur-2xl"
      />
      <Link
        to={orgHref(`/projects/${encodeURIComponent(project.id)}`)}
        aria-label={`Open project ${project.name}`}
        className="
          absolute inset-0 z-10 rounded-2xl
          focus:outline-none focus-visible:ring-2 focus-visible:ring-accent
        "
      />
      <button
        type="button"
        onClick={onDelete}
        aria-label={`Delete project ${project.name}`}
        className="
          absolute top-3 right-3 z-20
          inline-flex items-center justify-center
          h-7 w-7 rounded-full
          opacity-0 group-hover:opacity-100 focus-visible:opacity-100
          text-text-tertiary hover:text-dismiss hover:bg-dismiss/[0.08]
          focus:outline-none focus-visible:ring-2 focus-visible:ring-dismiss
          transition-[opacity,color,background-color] duration-200
        "
      >
        <Trash2 size={13} />
      </button>
      <div className="relative">
        <h3 className="text-[14px] font-semibold tracking-tight text-text-primary truncate pr-8">
          {project.name}
        </h3>
        {desc && (
          <p className="mt-2 text-[12px] leading-relaxed text-text-secondary line-clamp-3">
            {desc}
          </p>
        )}
        <div className="mt-3 text-[11px] text-text-tertiary tabular-nums">
          Updated {formatAge(project.updated_at)}
        </div>
      </div>
    </article>
  )
}

// formatAge keeps the card foot quiet — relative times for fresh
// updates, absolute dates after the activity has settled. Mirrors the
// shape Repos uses so users get the same temporal feel across pages.
function formatAge(iso: string): string {
  const t = Date.parse(iso)
  if (Number.isNaN(t)) return iso
  const diffMs = Date.now() - t
  const sec = Math.floor(diffMs / 1000)
  if (sec < 60) return 'just now'
  const min = Math.floor(sec / 60)
  if (min < 60) return `${min}m ago`
  const hr = Math.floor(min / 60)
  if (hr < 24) return `${hr}h ago`
  const day = Math.floor(hr / 24)
  if (day < 14) return `${day}d ago`
  return new Date(t).toLocaleDateString()
}
