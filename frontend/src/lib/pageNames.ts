// What each page the viewer has visited calls itself, by path, so something
// reported about a page after the viewer has left it can name it: a failed
// request's toast links back by this name. A page outside the shell's frame
// (the run station) names nothing, and is linked to without a name.

/** Paths remembered at most; the oldest is forgotten first. */
const LIMIT = 50

const names = new Map<string, string>()

/** Records that the page at `pathname` is called `name`; an empty name forgets
 *  what was recorded, so a page that names itself nothing is not called by
 *  whatever was recorded for its path before. */
export function rememberPageName(pathname: string, name: string) {
  names.delete(pathname)
  if (name === '') return
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
