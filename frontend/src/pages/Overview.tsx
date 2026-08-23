// Overview — a stub, deliberately.
//
// The rail's first row and the scope switcher's header both point here, so the
// route has to exist before the page does; a nav row that 404s is worse than
// one that says "not yet". The page itself arrives separately.
//
// It states what it will be rather than showing an empty frame, because an
// empty frame reads as a page that failed to load.
//
// TODO(TFAC-893): build the page this route promises; the stub and its
// self-description go with it.
export default function Overview() {
  return (
    <div className="max-w-measure">
      <p className="text-body text-ink-2 leading-prose">
        The overview lands here — the shape of the work in flight, across every source this team
        tracks.
      </p>
      <p className="text-secondary text-ink-4 leading-prose mt-4">
        Not built yet. The rail row and the scope switcher both point at this route already, so it
        exists to be arrived at rather than to be read.
      </p>
    </div>
  )
}
