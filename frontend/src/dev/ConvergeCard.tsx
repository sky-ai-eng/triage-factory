import { useState } from 'react'
import { Converge } from '../ui/converge/Converge'
import { FlapCount } from '../ui/flapcount/FlapCount'

// The Converge harness. Click the fan to replay the build — the beats are the
// review. Toggle the band to see the endpoints compress toward the floor
// (clearing the corner a masthead would take), and note the strand counts
// holding a picture even though `filtered` dwarfs everything: strands are
// structure, and the counts on the right are the data.

export function ConvergeCard() {
  const [band, setBand] = useState(false)
  const [events, setEvents] = useState(312)
  const [filtered, setFiltered] = useState(296)

  return (
    <div className="gal-card">
      <div className="gal-cardhead">
        <span>Converge</span>
        <span style={{ display: 'inline-flex', gap: 6 }}>
          <button type="button" className="gal-btn" onClick={() => setBand((b) => !b)}>
            {band ? 'full band' : 'narrowed band'}
          </button>
          <button
            type="button"
            className="gal-btn"
            onClick={() => {
              setEvents((n) => n + 7)
              setFiltered((n) => n + 7)
            }}
          >
            +7 events
          </button>
        </span>
      </div>

      <p className="gal-note">
        Many to few. The build plays on arrival and on click, never on data change — press +7 and
        only the headline&rsquo;s FlapCount moves, because a value that ticks moves in place. The
        headline is <code>titleNode</code> (the self-animating figure) beside a decoded string; the
        band compresses every lane through one <code>yFor</code> so labels can never slide off their
        endpoints.
      </p>

      <div style={{ position: 'relative', height: 300 }}>
        <Converge
          kicker="SINCE MIDNIGHT"
          title="events triaged"
          titleNode={<FlapCount value={events} size={24} label={events + ' events triaged'} />}
          outcomes={[
            { name: 'merged', v: 6, tone: 'warm' },
            { name: 'running', v: 3, tone: 'cool' },
            { name: 'need you', v: 7, tone: 'ask' },
            { name: 'filtered by rules', v: filtered, tone: 'quiet' },
          ]}
          strands={28}
          height={260}
          fill
          endpointBand={band ? [0.3, 1] : [0, 1]}
        />
      </div>
    </div>
  )
}

export default ConvergeCard
