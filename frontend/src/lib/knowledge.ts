import { apiFetch, apiList } from './apiClient'
import type { ListPage } from './apiClient'

/**
 * The team knowledge base, from the client side.
 *
 * One KB per team, two roots. `private/` is the team's own; `shared/` is what
 * they have published to the rest of the organization. **Visibility is the
 * root, not a flag** — there is no per-file setting anywhere, and publishing is
 * `moveKnowledge` from one root to the other. That is why the page has two
 * tabs and one verb rather than a visibility column with a dropdown in it.
 *
 * Every read goes through the `POST /…/list` convention, like every other
 * paginated read in the app. The project-era KB panel issued a GET against a
 * route that only ever registered POST, so it silently rendered nothing; going
 * through `apiList` is what keeps that from being possible.
 */

/** The two visibility roots. A closed vocabulary — the server rejects anything else. */
export type KnowledgeRoot = 'private' | 'shared'

export const KNOWLEDGE_ROOTS: KnowledgeRoot[] = ['private', 'shared']

/** One document in a team's knowledge base, as the list read returns it. */
export interface KnowledgeFile {
  root: KnowledgeRoot
  /** Slash-separated path under the root — folders are prefixes, not rows. */
  path: string
  /** The leaf name, so a row renders without re-splitting the path. */
  name: string
  size: number
  mod_time: string
}

/** What one uploaded file became. `outcome` is why the page can say "replaced"
 *  rather than reporting every write as a new document. */
export interface KnowledgeUploadResult {
  root: KnowledgeRoot
  path: string
  name: string
  size: number
  outcome: 'created' | 'replaced'
}

interface KnowledgeUploadResponse {
  files: KnowledgeUploadResult[]
}

/** How many entries the move verb touched — one for a document, the subtree's
 *  size for a folder. */
interface KnowledgeMoveResponse {
  moved: number
}

/** How many entries the delete verb removed. Its own field name rather than the
 *  move's, because one name across two outcomes eventually gets rendered as the
 *  wrong one. */
interface KnowledgeDeleteResponse {
  removed: number
}

function base(teamId: string): string {
  return `/api/teams/${encodeURIComponent(teamId)}/kb`
}

/**
 * One page of a team's knowledge.
 *
 * `root` narrows to one root; omitting it asks for every root the caller may
 * read, which is both for a member and `shared` alone for anyone else in the
 * org. The server resolves that from membership — a client never has to know
 * which it is getting, and asking for a root it cannot see answers an empty
 * page rather than an error.
 */
export function listKnowledge(
  teamId: string,
  filters: { root?: KnowledgeRoot; path_prefix?: string; page_size?: number; page_token?: string },
  signal?: AbortSignal,
): Promise<ListPage<KnowledgeFile>> {
  return apiList<KnowledgeFile>(`${base(teamId)}/list`, filters, { signal })
}

/** The URL a document is read at — the same `<root>/<path>` a listing row
 *  carries. Each segment is encoded on its own so a folder separator survives
 *  as a separator and a space in a name does not. */
export function knowledgeFileUrl(teamId: string, root: KnowledgeRoot, path: string): string {
  const encoded = path
    .split('/')
    .map((seg) => encodeURIComponent(seg))
    .join('/')
  return `${base(teamId)}/${root}/${encoded}`
}

/** Fetch one document's text. Used by the inline viewer; the same URL is what
 *  the download link points at. */
export async function readKnowledgeFile(
  teamId: string,
  root: KnowledgeRoot,
  path: string,
  signal?: AbortSignal,
): Promise<string> {
  const resp = await apiFetch(knowledgeFileUrl(teamId, root, path), { signal })
  return resp.text()
}

/**
 * Upload one or more files into a root, optionally under a folder.
 *
 * multipart because the payload is file bytes. The fields are written before
 * the parts on purpose: the server streams the body and has to know which root
 * it is writing into before the first file arrives.
 *
 * A file whose `webkitRelativePath` is set — what a browser sends for a folder
 * drop — keeps that shape under the prefix, so an uploaded directory arrives as
 * a directory.
 */
export async function uploadKnowledge(
  teamId: string,
  root: KnowledgeRoot,
  files: File[],
  pathPrefix = '',
): Promise<KnowledgeUploadResult[]> {
  const form = new FormData()
  form.append('root', root)
  if (pathPrefix) form.append('path_prefix', pathPrefix)
  for (const file of files) {
    const rel = (file as File & { webkitRelativePath?: string }).webkitRelativePath
    form.append('files', file, rel || file.name)
  }
  const resp = await apiFetch(`${base(teamId)}/files`, { method: 'POST', body: form })
  const out = (await resp.json()) as KnowledgeUploadResponse
  return out.files ?? []
}

/**
 * Move a document or a folder.
 *
 * This is publish, unpublish, rename and reorganize — all four are the same
 * transition on a two-root layout, so there is one verb rather than four
 * near-identical calls. It refuses on a collision (409) rather than
 * overwriting, so a publish either lands whole or does not land.
 */
export async function moveKnowledge(
  teamId: string,
  from: { root: KnowledgeRoot; path: string },
  to: { root: KnowledgeRoot; path: string },
): Promise<number> {
  const resp = await apiFetch(`${base(teamId)}/move`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      from_root: from.root,
      from_path: from.path,
      to_root: to.root,
      to_path: to.path,
    }),
  })
  const out = (await resp.json()) as KnowledgeMoveResponse
  return out.moved ?? 0
}

/** Remove a document, or a folder and everything under it. Returns how many
 *  entries went away. */
export async function deleteKnowledge(
  teamId: string,
  root: KnowledgeRoot,
  path: string,
): Promise<number> {
  const resp = await apiFetch(knowledgeFileUrl(teamId, root, path), { method: 'DELETE' })
  const out = (await resp.json()) as KnowledgeDeleteResponse
  return out.removed ?? 0
}

/** The other root — where publishing sends a document, and where unpublishing
 *  brings it back. */
export function otherRoot(root: KnowledgeRoot): KnowledgeRoot {
  return root === 'private' ? 'shared' : 'private'
}

/** The folder part of a path, or '' at the root of a tree. */
export function folderOf(path: string): string {
  const at = path.lastIndexOf('/')
  return at < 0 ? '' : path.slice(0, at)
}

/** A size a person reads. Bytes below a kilobyte stay bytes — a runbook that
 *  is 340 bytes is a stub, and "0.3 KB" hides that. */
export function fmtBytes(n: number): string {
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(n < 10 * 1024 ? 1 : 0)} KB`
  return `${(n / (1024 * 1024)).toFixed(1)} MB`
}
