import { inputClass } from './primitives'
import { API_TOKEN_MAX_AGE_DAYS_MAX, API_TOKEN_MAX_AGE_DAYS_MIN } from './orgConfig'

// The org's ceiling on API-token lifetime — the body of the "API token lifetime"
// settings section, extracted so the copy that has to state the control's two
// consequences is one thing with one test rather than a paragraph buried in a
// thousand-line page.
//
// The copy is the point. A cap is applied at USE, not stamped into a token at
// mint, so it is not a rule about tokens minted from now on: lowering it
// shortens every token the org already holds, and raising it lengthens the ones
// whose minter did not pick an earlier expiry themselves. Neither is softened
// or confirmed — an admin who lowers the cap is doing exactly what the control
// is for — but both are said plainly, because the surprising one costs somebody
// a broken deployment.
export default function ApiTokenPolicyGroup({
  value,
  onChange,
  error,
}: {
  value: string
  onChange: (value: string) => void
  error: string | null
}) {
  return (
    <div className="space-y-5">
      <div className="space-y-1.5">
        <h2 className="text-[19px] font-medium tracking-tight text-ink-1">
          Cap how long API tokens live
        </h2>
        <p className="text-body leading-relaxed text-ink-3">
          The longest any personal API token in this organization may be used for, counted from when
          it was created. The limit applies to tokens that already exist, not just new ones:
          lowering it immediately shortens every token in the organization, and one already older
          than the new limit stops working on its next request. Raising or removing it extends only
          the tokens whose creator didn&rsquo;t pick an earlier expiry of their own. Leave blank for
          no maximum.
        </p>
      </div>
      <label className="block max-w-[220px]">
        <span className="mb-1.5 block text-reported text-ink-3">Maximum age (days)</span>
        <input
          type="number"
          min={API_TOKEN_MAX_AGE_DAYS_MIN}
          max={API_TOKEN_MAX_AGE_DAYS_MAX}
          step="1"
          inputMode="numeric"
          placeholder="No maximum"
          aria-invalid={error !== null}
          aria-describedby={error !== null ? 'api-token-max-age-error' : undefined}
          value={value}
          onChange={(e) => onChange(e.target.value)}
          className={`${inputClass} ${error !== null ? 'border-alarm/60 focus:border-alarm/60' : ''}`}
        />
        {error !== null && (
          <span id="api-token-max-age-error" className="mt-1.5 block text-reported text-alarm">
            {error}
          </span>
        )}
      </label>
    </div>
  )
}
