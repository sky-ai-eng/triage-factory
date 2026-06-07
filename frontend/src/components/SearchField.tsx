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
        className="pointer-events-none absolute left-0 top-1/2 -translate-y-1/2 text-text-tertiary"
      />
      <input
        type="text"
        aria-label={ariaLabel}
        placeholder={placeholder}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        className="w-full border-0 border-b border-border-subtle bg-transparent py-2.5 pl-6 pr-2 text-[14px] text-text-primary placeholder:text-text-tertiary outline-none transition-colors focus:border-accent"
      />
    </div>
  )
}
