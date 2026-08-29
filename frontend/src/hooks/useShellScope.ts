import { useOutletContext } from 'react-router'

// The shell's org · team mark is the scope control for the pages that carry no
// scope control of their own (the Overview, by design). The Shell adapter owns
// the selection — one instance, persisted, seeded like every single-team page
// scope — and shares the resolved team through the router outlet, so switching
// team in the rail reframes the page in the same render that moves the mark.

export type ShellScope = {
  /** The resolved scope team's id — concrete once the teams list has loaded
   *  (the sole team in local / single-team orgs), '' before it. */
  teamId: string
  teamName: string
}

const NO_SCOPE: ShellScope = { teamId: '', teamName: '' }

export function useShellScope(): ShellScope {
  // Null when rendered outside the framed shell (the full-bleed run station
  // mounts its outlet bare) — pages that read the scope only mount framed,
  // but a hook must not throw for being asked somewhere else.
  return useOutletContext<ShellScope | null>() ?? NO_SCOPE
}
