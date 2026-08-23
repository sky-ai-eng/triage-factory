import { Search } from 'lucide-react'

// SearchField is the flush, borderless search input for the setup wizard's
// pickers (repos, GitHub teams): a leading magnifier over a transparent field
// with just a bottom hairline that lights accent on focus — sleeker and more
// blended than a boxed input. The carded surfaces (Settings) keep their boxed
// search.
export default function SearchField({
  value,
  onChange,
  placeholder,
  ariaLabel,
}: {
  value: string
  onChange: (value: string) => void
  placeholder: string
  ariaLabel: string
}) {
  return (
    <div className="relative">
      <Search
        size={15}
        aria-hidden
        className="pointer-events-none absolute left-0 top-1/2 -translate-y-1/2 text-ink-3"
      />
      <input
        type="text"
        aria-label={ariaLabel}
        placeholder={placeholder}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        className="w-full border-0 border-b border-line-1 bg-transparent py-2.5 pl-6 pr-2 text-body text-ink-1 placeholder:text-ink-3 outline-none transition-colors focus:border-warm"
      />
    </div>
  )
}
