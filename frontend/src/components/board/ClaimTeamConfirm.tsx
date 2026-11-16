import { useEffect, useRef, useState } from 'react'
import { Dialog } from '../../ui/dialog/Dialog'
import { Segmented } from '../../ui/segmented/Segmented'
import { useTeams } from '../../hooks/useTeams'
import TeamPicker from '../TeamPicker'

// Segmented shows two to five options; past that the candidates go in a
// TeamPicker limited to them.
const SEGMENTED_MAX = 5

// ClaimTeamConfirm asks which team a claim lands a task on, and it appears
// only when the question exists: more than one of the caller's teams can take
// the task. With one candidate the claim goes out without it. The panel
// arrives already built because the question is routine, and the verb is a
// press: a claim is undone by returning the task to the queue.
//
// The choice starts at `preselected` each time the dialog opens. Cancel,
// Escape and the backdrop all call onCancel, and the caller writes nothing.
export default function ClaimTeamConfirm({
  open,
  candidates,
  preselected,
  onConfirm,
  onCancel,
}: {
  open: boolean
  /** The teams the claim may land on (the task's `claim_team_ids`). */
  candidates: string[]
  /** The candidate chosen when the dialog opens. */
  preselected: string
  onConfirm: (teamId: string) => void
  onCancel: () => void
}) {
  const { teams } = useTeams()
  const [team, setTeam] = useState(preselected)
  // Reset at render when the dialog opens, so a reopened dialog never shows
  // the last task's choice for a frame.
  const [wasOpen, setWasOpen] = useState(open)
  if (open !== wasOpen) {
    setWasOpen(open)
    if (open) setTeam(preselected)
  }
  // A press, not a hold, so a fast double-click could land both clicks on the
  // confirm button before the caller closes the dialog. The latch makes the
  // second one a no-op instead of a second claim. Rearmed on each open.
  const firedRef = useRef(false)
  useEffect(() => {
    if (open) firedRef.current = false
  }, [open])

  const names = new Map(teams.map((t) => [t.id, t.name]))

  return (
    <Dialog
      open={open}
      kind="normal"
      build="none"
      title="Which team takes this task?"
      body="More than one of your teams can take it. Your chat on it, and what it spends, belong to the team you pick."
      confirmLabel="Claim"
      cancelLabel="Cancel"
      onConfirm={() => {
        if (firedRef.current) return
        firedRef.current = true
        onConfirm(team)
      }}
      onCancel={onCancel}
    >
      {candidates.length > SEGMENTED_MAX ? (
        <TeamPicker value={team} onChange={setTeam} teamIds={candidates} />
      ) : (
        <Segmented
          label="Team"
          mono={false}
          options={candidates.map((id) => ({ value: id, label: names.get(id) ?? id }))}
          value={team}
          onChange={setTeam}
        />
      )}
    </Dialog>
  )
}
