// What each page the viewer has visited calls itself, by path, so something
// reported about a page after the viewer has left it can name it: a failed
// request's toast links back by this name. A page names itself through its
// header (usePageHeader), which records it here.

/** Paths remembered at most; the oldest is forgotten first. */
const LIMIT = 50

const names = new Map<string, string>()

/** Records that the page at `pathname` is called `name`. */
export function rememberPageName(pathname: string, name: string) {
  names.delete(pathname)
  names.set(pathname, name)
  if (names.size > LIMIT) {
    const oldest = names.keys().next().value
    if (oldest !== undefined) names.delete(oldest)
  }
}

/** What the page at `path` (a query is ignored) called itself, if it did. */
export function pageNameFor(path: string): string | undefined {
  return names.get(path.split('?')[0])
}
