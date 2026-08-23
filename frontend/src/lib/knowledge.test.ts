import { describe, it, expect, vi, beforeEach } from 'vitest'

// The request shapes behind the Knowledge page. They live here rather than in
// the page test because what can go wrong here is not visible on screen: a
// transposed from/to pair silently unpublishes what someone meant to publish,
// and an unencoded path addresses a document that is not the one they clicked.

const api = vi.hoisted(() => ({ apiFetch: vi.fn(), apiList: vi.fn() }))
vi.mock('./apiClient', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./apiClient')>()
  return { ...actual, ...api }
})

import {
  deleteKnowledge,
  fmtBytes,
  folderOf,
  knowledgeFileUrl,
  listKnowledge,
  moveKnowledge,
  otherRoot,
  uploadKnowledge,
} from './knowledge'

function jsonResponse(body: unknown) {
  return {
    ok: true,
    status: 200,
    json: () => Promise.resolve(body),
    text: () => Promise.resolve(JSON.stringify(body)),
  } as unknown as Response
}

beforeEach(() => {
  vi.clearAllMocks()
  api.apiFetch.mockResolvedValue(jsonResponse({ moved: 1 }))
  api.apiList.mockResolvedValue({ items: [], next_page_token: '', total_count: 0 })
})

describe('listKnowledge', () => {
  it('reads through POST /list, the convention every paginated read speaks', async () => {
    await listKnowledge('t1', { root: 'shared', path_prefix: 'runbooks', page_size: 200 })
    expect(api.apiList).toHaveBeenCalledWith(
      '/api/teams/t1/kb/list',
      { root: 'shared', path_prefix: 'runbooks', page_size: 200 },
      expect.anything(),
    )
  })
})

describe('knowledgeFileUrl', () => {
  it('encodes each segment, so a separator stays a separator and a space does not', () => {
    expect(knowledgeFileUrl('t1', 'private', 'runbooks/Deploy Guide.md')).toBe(
      '/api/teams/t1/kb/private/runbooks/Deploy%20Guide.md',
    )
    expect(knowledgeFileUrl('t 1', 'shared', 'a#b.md')).toBe('/api/teams/t%201/kb/shared/a%23b.md')
  })
})

describe('moveKnowledge', () => {
  it('sends from and to as the pair the server takes them apart as', async () => {
    await moveKnowledge(
      't1',
      { root: 'private', path: 'runbooks' },
      { root: 'shared', path: 'runbooks' },
    )
    const [path, opts] = api.apiFetch.mock.calls[0]
    expect(path).toBe('/api/teams/t1/kb/move')
    expect(opts.method).toBe('POST')
    expect(JSON.parse(String(opts.body))).toEqual({
      from_root: 'private',
      from_path: 'runbooks',
      to_root: 'shared',
      to_path: 'runbooks',
    })
  })

  it('reports how many entries the transition touched', async () => {
    api.apiFetch.mockResolvedValueOnce(jsonResponse({ moved: 4 }))
    expect(
      await moveKnowledge('t1', { root: 'private', path: 'r' }, { root: 'shared', path: 'r' }),
    ).toBe(4)
  })
})

describe('deleteKnowledge', () => {
  it('addresses the document by the same <root>/<path> a listing row carries', async () => {
    api.apiFetch.mockResolvedValueOnce(jsonResponse({ removed: 2 }))
    const removed = await deleteKnowledge('t1', 'private', 'docs/sub/a.md')
    const [path, opts] = api.apiFetch.mock.calls[0]
    expect(path).toBe('/api/teams/t1/kb/private/docs/sub/a.md')
    expect(opts.method).toBe('DELETE')
    expect(removed).toBe(2)
  })
})

describe('uploadKnowledge', () => {
  it('writes the root before the files, because the server streams the body', async () => {
    api.apiFetch.mockResolvedValueOnce(jsonResponse({ files: [] }))
    await uploadKnowledge('t1', 'shared', [new File(['x'], 'a.md')], 'runbooks')

    const form = api.apiFetch.mock.calls[0][1].body as FormData
    const keys = [...form.keys()]
    expect(keys.indexOf('root')).toBeLessThan(keys.indexOf('files'))
    expect(form.get('root')).toBe('shared')
    expect(form.get('path_prefix')).toBe('runbooks')
  })

  it('omits an empty folder rather than sending a blank one', async () => {
    api.apiFetch.mockResolvedValueOnce(jsonResponse({ files: [] }))
    await uploadKnowledge('t1', 'private', [new File(['x'], 'a.md')])
    const form = api.apiFetch.mock.calls[0][1].body as FormData
    expect(form.get('path_prefix')).toBeNull()
  })

  it("keeps a folder drop's relative path, so an uploaded directory arrives as one", async () => {
    api.apiFetch.mockResolvedValueOnce(jsonResponse({ files: [] }))
    const file = new File(['x'], 'deploy.md')
    Object.defineProperty(file, 'webkitRelativePath', { value: 'runbooks/deploy.md' })
    await uploadKnowledge('t1', 'private', [file])

    const form = api.apiFetch.mock.calls[0][1].body as FormData
    const sent = form.get('files') as File
    expect(sent.name).toBe('runbooks/deploy.md')
  })

  it('reports what each file became', async () => {
    api.apiFetch.mockResolvedValueOnce(
      jsonResponse({
        files: [{ root: 'private', path: 'a.md', name: 'a.md', size: 1, outcome: 'replaced' }],
      }),
    )
    const out = await uploadKnowledge('t1', 'private', [new File(['x'], 'a.md')])
    expect(out).toEqual([
      { root: 'private', path: 'a.md', name: 'a.md', size: 1, outcome: 'replaced' },
    ])
  })
})

describe('helpers', () => {
  it('otherRoot is the publish/unpublish direction', () => {
    expect(otherRoot('private')).toBe('shared')
    expect(otherRoot('shared')).toBe('private')
  })

  it('folderOf stops at the last separator', () => {
    expect(folderOf('a/b/c.md')).toBe('a/b')
    expect(folderOf('c.md')).toBe('')
  })

  it('fmtBytes keeps a small document in bytes, where the number is the point', () => {
    expect(fmtBytes(340)).toBe('340 B')
    expect(fmtBytes(2048)).toBe('2.0 KB')
    expect(fmtBytes(200 * 1024)).toBe('200 KB')
    expect(fmtBytes(3 * 1024 * 1024)).toBe('3.0 MB')
  })
})
