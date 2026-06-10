// stripWorktree collapses the agent's absolute worktree paths down to paths
// relative to the run's worktree root, so run surfaces show
// "frontend/src/index.css" instead of the
// /private/var/folders/.../triagefactory-runs/<id>/frontend/src/index.css
// mouthful. It runs over arbitrary strings (bash commands, JSON args, output),
// not just structured file_path fields, since paths are embedded inside
// commands. macOS resolves the temp root through the /private symlink, so the
// agent's paths may carry a /private prefix the stored worktree path omits (or
// vice versa) — we match both, longest first so the /private form is consumed
// before the bare path it contains.
//
// A bare match (the root itself, no trailing slash) deliberately renders as
// "." — the shell idiom for "right here" — so `cd <root>` reads as `cd .` and
// a file_path equal to the root reads as the working directory, not as an
// empty string.
export function stripWorktree(text: string, worktree?: string): string {
  if (!worktree || !text) return text
  const wt = worktree.replace(/\/+$/, '')
  const bare = wt.startsWith('/private/') ? wt.slice('/private'.length) : wt
  const variants = Array.from(new Set([`/private${bare}`, wt, bare])).sort(
    (a, b) => b.length - a.length,
  )
  let out = text
  for (const v of variants) {
    out = out.split(`${v}/`).join('').split(v).join('.')
  }
  return out
}
