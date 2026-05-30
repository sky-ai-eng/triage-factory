// TeamTag is the small per-team chip the multi-team board uses to
// color-code / tag task rows. The color is derived
// deterministically from the team id so the same team always reads the
// same hue across the board without any server-supplied palette — a
// stable, collision-tolerant visual grouping rather than a precise
// identity color.
interface Props {
  id: string
  name: string
}

// hueFromId folds the id's char codes into a 0–359 hue. Deterministic
// and cheap; distinct teams scatter across the wheel well enough for
// at-a-glance grouping.
function hueFromId(id: string): number {
  let h = 0
  for (let i = 0; i < id.length; i++) {
    h = (h * 31 + id.charCodeAt(i)) % 360
  }
  return h
}

export default function TeamTag({ id, name }: Props) {
  const hue = hueFromId(id)
  return (
    <span
      className="inline-flex max-w-[8rem] items-center gap-1 truncate rounded-full px-1.5 py-0.5 text-[10px] font-medium"
      style={{
        backgroundColor: `hsl(${hue} 70% 94%)`,
        color: `hsl(${hue} 55% 32%)`,
      }}
      title={`Team: ${name}`}
    >
      <span
        className="inline-block h-1.5 w-1.5 shrink-0 rounded-full"
        style={{ backgroundColor: `hsl(${hue} 60% 50%)` }}
      />
      <span className="truncate">{name}</span>
    </span>
  )
}
