// The Claude-credentials bodies — the org-level "how do runs reach Claude"
// flow, shared by the setup wizard's two steps (org-claude-source +
// org-claude-key) and the Settings "Claude credentials" section, so both
// surfaces read identically:
//
//   OrgClaudeSourceStep — local-only source picker: "Use system Claude Code
//                         credentials" vs "Bring your own API key." A flush
//                         two-panel ChoiceCards, action-on-click (selfAdvancing).
//                         Multi never shows this (no system-creds option —
//                         that'd be cross-tenant bleed), jumping straight to the
//                         key step.
//   OrgClaudeKeyStep    — the provider + credential step: an Anthropic /
//                         Amazon Bedrock radio, then the provider's credential
//                         fields. Continue drives the provider's validated
//                         connect endpoint and blocks on failure.
//
// Reuse rule: composes the shared ChoiceCards picker and the wizard's glass
// field primitives — no parallel field UIs. ClaudeProviderCards,
// AnthropicKeyField, and BedrockFields are exported so the Settings section
// renders the same provider radio + credential inputs.

import { useState } from 'react'
import { Check, ChevronRight, Copy } from 'lucide-react'
import { glassInputClass } from '../settings/primitives'
import { CLAUDE_SOURCE_OPTIONS } from '../settings/anthropicConnect'
import { BEDROCK_AUTH_OPTIONS, fetchBedrockRoleSetup } from '../settings/bedrockConnect'
import type { BedrockAuthMethod, OrgConfigForm } from '../settings/orgConfig'
import { ChoiceCards } from './parts'
import type { ClaudeProvider, StepContext } from './types'

// OrgClaudeSourceStep body — the local-only source picker. Action-on-click (the
// step is selfAdvancing): choosing "system" records it and advances (the step's
// persist clears any stored key); "byok" advances to the key step.
export function OrgClaudeSourceStep({ state, patch, advance }: StepContext) {
  const choose = (kind: 'system' | 'byok') => {
    patch({ anthropicKeySource: kind })
    advance?.()
  }
  return (
    <div className="space-y-5">
      <div className="space-y-1.5">
        <h2 className="text-[19px] font-medium tracking-tight text-ink-1">
          How should runs reach Claude?
        </h2>
        <p className="text-body leading-relaxed text-ink-3">
          Triage Factory delegates work to Claude Code. Use the credentials already on this machine,
          or bring your own Anthropic or Amazon Bedrock credentials.
        </p>
      </div>
      <ChoiceCards
        ariaLabel="Claude credential source"
        options={CLAUDE_SOURCE_OPTIONS}
        selected={state.anthropicKeySource}
        onChoose={choose}
      />
    </div>
  )
}

interface ProviderCard {
  kind: ClaudeProvider
  title: string
  blurb: string
}

const PROVIDER_CARDS: ProviderCard[] = [
  { kind: 'anthropic', title: 'Anthropic API', blurb: 'A direct Anthropic API key (sk-ant-…).' },
  {
    kind: 'bedrock',
    title: 'Amazon Bedrock',
    blurb: 'Claude via your AWS account — a Bedrock API key or IAM access keys.',
  },
]

// ClaudeProviderCards is the provider radio: Anthropic direct or Amazon
// Bedrock. Selecting a provider swaps the credential fields below it; the
// stored provider is whichever connect endpoint last succeeded (the backend
// keeps them mutually exclusive). Shared by the wizard key step and the
// Settings section.
export function ClaudeProviderCards({
  selected,
  onSelect,
}: {
  selected: ClaudeProvider
  onSelect: (kind: ClaudeProvider) => void
}) {
  return (
    <div role="radiogroup" aria-label="Claude provider" className="grid gap-2 sm:grid-cols-2">
      {PROVIDER_CARDS.map((card) => {
        const isSelected = card.kind === selected
        return (
          <button
            key={card.kind}
            type="button"
            role="radio"
            aria-checked={isSelected}
            onClick={() => onSelect(card.kind)}
            className={`flex flex-col items-start gap-1 rounded-xl border px-3.5 py-3 text-left transition-colors ${
              isSelected
                ? 'border-warm/50 bg-warm/[0.06] shadow-float shadow-black/[0.03]'
                : 'border-line-1 bg-tint-2 hover:border-border-strong'
            }`}
          >
            <span
              className={`text-body font-medium ${
                isSelected ? 'text-warm' : 'text-ink-1'
              }`}
            >
              {card.title}
            </span>
            <span className="text-reported leading-snug text-ink-3">{card.blurb}</span>
          </button>
        )
      })}
    </div>
  )
}

// secretField / textField are the shared label + input scaffolding for the
// credential inputs, matching AnthropicKeyField's visual shape so every
// provider's fields read identically.
function LabeledInput({
  label,
  value,
  onChange,
  placeholder,
  secret,
  invalid,
}: {
  label: string
  value: string
  onChange: (value: string) => void
  placeholder: string
  secret?: boolean
  invalid?: boolean
}) {
  return (
    <label className="block">
      <span className="mb-2 block text-reported font-medium uppercase tracking-wide text-ink-3">
        {label}
      </span>
      <input
        type={secret ? 'password' : 'text'}
        value={value}
        placeholder={placeholder}
        onChange={(e) => onChange(e.target.value)}
        aria-invalid={invalid || undefined}
        autoComplete="off"
        className={`${glassInputClass}${
          invalid
            ? ' !border-[var(--color-alarm)] focus:!border-[var(--color-alarm)] focus:!shadow-[0_0_0_4px_rgba(168,69,69,0.16)]'
            : ''
        }`}
      />
    </label>
  )
}

// AnthropicKeyField is the API-key paste input — a password field with the
// "leave blank to keep current" secret convention when a key is already stored.
// `invalid` reddens the border (the wizard threads its persist error here); the
// message itself renders in the host's shared error line. Shared so Settings
// shows the identical field.
export function AnthropicKeyField({
  value,
  onChange,
  hasKey,
  invalid,
}: {
  value: string
  onChange: (value: string) => void
  hasKey: boolean
  invalid?: boolean
}) {
  return (
    <div className="space-y-2">
      <LabeledInput
        label={`Anthropic API key${hasKey ? ' (leave blank to keep current)' : ''}`}
        value={value}
        onChange={onChange}
        placeholder={hasKey ? '••••••••' : 'sk-ant-…'}
        secret
        invalid={invalid}
      />
      <p className="text-reported leading-relaxed text-ink-3">
        Create one in the{' '}
        <a
          href="https://console.anthropic.com/settings/keys"
          target="_blank"
          rel="noopener noreferrer"
          className="text-warm hover:underline"
        >
          Anthropic Console
        </a>
        . We check it with Anthropic before saving.
      </p>
      <KeyHardeningChecklist />
    </div>
  )
}

// KeyHardeningChecklist is the "harden your key" nudge under the Anthropic key
// input (shared, so it shows in both the wizard and Settings): a compact list
// of the three cheap protections against a leaked key, each expandable to a
// one-line "why" plus a deep link to the relevant console page. Native
// <details>/<summary> so there's no new dependency or open-state plumbing.
function KeyHardeningChecklist() {
  return (
    <div className="space-y-1 rounded-lg border border-line-1 bg-tint-2 px-3 py-2">
      <p className="text-reported font-medium text-ink-3">Before you save, harden this key</p>
      <ul className="space-y-0.5">
        <HardeningItem
          title="Use a dedicated workspace with a spend limit"
          why="A scoped workspace with a monthly cap bounds how much a leaked key can spend."
          href="https://console.anthropic.com/settings/workspaces"
          linkText="Workspaces"
        />
        <HardeningItem
          title="Set an expiration on the key"
          why="An expiry bounds the window a leaked or forgotten key stays usable."
          href="https://console.anthropic.com/settings/keys"
          linkText="API keys"
        />
        <HardeningItem
          title="Know how to revoke it"
          why="If the key leaks, delete it from the console — revocation is immediate."
          href="https://console.anthropic.com/settings/keys"
          linkText="API keys"
        />
      </ul>
    </div>
  )
}

// HardeningItem — one checklist line: the recommendation as the <summary>, its
// "why" + a deep link revealed on expand. A rotating chevron replaces the
// native disclosure marker (list-none) to match the wizard's ChoiceCards idiom.
function HardeningItem({
  title,
  why,
  href,
  linkText,
}: {
  title: string
  why: string
  href: string
  linkText: string
}) {
  return (
    <li className="text-reported leading-relaxed text-ink-3">
      <details className="group">
        <summary className="flex cursor-pointer list-none items-center gap-1 marker:content-none">
          <ChevronRight
            size={11}
            aria-hidden
            className="shrink-0 text-ink-3 transition-transform group-open:rotate-90"
          />
          <span>{title}</span>
        </summary>
        <p className="mt-0.5 pl-[15px] text-ink-3/85">
          {why}{' '}
          <a
            href={href}
            target="_blank"
            rel="noopener noreferrer"
            className="text-warm hover:underline"
          >
            {linkText}
          </a>
        </p>
      </details>
    </li>
  )
}

// CopyButton — the shared Copy/Copied affordance used by the role setup's
// read-only External ID field and trust-policy block. Matches the settings
// CopyField idiom (SSOSettings): swap the icon + label for ~1.5s on success,
// silently no-op if the clipboard is denied (the value is still selectable).
function CopyButton({ value, label }: { value: string; label: string }) {
  const [copied, setCopied] = useState(false)
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(value)
      setCopied(true)
      setTimeout(() => setCopied(false), 1500)
    } catch {
      // Clipboard denied — the value is still selectable.
    }
  }
  return (
    <button
      type="button"
      onClick={() => void copy()}
      aria-label={`Copy ${label}`}
      className="inline-flex shrink-0 items-center gap-1 rounded-xl border border-[var(--color-line-1)] px-3 py-2 text-ui font-medium text-ink-2 transition-colors hover:text-ink-1"
    >
      {copied ? <Check size={13} /> : <Copy size={13} />}
      {copied ? 'Copied' : 'Copy'}
    </button>
  )
}

// BedrockRoleFields — the IAM-role method body: the editable Role ARN, the
// read-only (server-generated) External ID, and a manual "generate setup"
// action that POSTs /api/bedrock/role-setup and renders the returned trust
// policy in a copyable block. Manual (a button, not an on-mount effect) so a
// controlled component stays effect-free and the wizard + Settings share the
// exact same behavior. The External ID lands in form state via onChange so it
// round-trips with the rest of the Bedrock config on connect.
function BedrockRoleFields({
  form,
  onChange,
  invalid,
}: {
  form: OrgConfigForm
  onChange: (patch: Partial<OrgConfigForm>) => void
  invalid?: boolean
}) {
  const [loading, setLoading] = useState(false)
  const [setupError, setSetupError] = useState<string | null>(null)
  const [trustPolicy, setTrustPolicy] = useState('')
  const [callerArn, setCallerArn] = useState('')

  const runSetup = async () => {
    setLoading(true)
    setSetupError(null)
    const r = await fetchBedrockRoleSetup()
    setLoading(false)
    if (!r.ok) {
      setSetupError(r.error)
      return
    }
    setTrustPolicy(r.trust_policy)
    setCallerArn(r.caller_identity_arn)
    onChange({ bedrock_external_id: r.external_id })
  }

  return (
    <div className="space-y-3">
      <LabeledInput
        label="Role ARN"
        value={form.bedrock_role_arn}
        onChange={(v) => onChange({ bedrock_role_arn: v })}
        placeholder="arn:aws:iam::123456789012:role/tf-bedrock"
        invalid={invalid && form.bedrock_role_arn.trim() === ''}
      />
      {/* External ID — read-only, generated by the setup call; the role's trust
          policy must pin it (the confused-deputy guard). */}
      <div className="space-y-1.5">
        <span className="block text-reported font-medium uppercase tracking-wide text-ink-3">
          External ID
        </span>
        {form.bedrock_external_id ? (
          <div className="flex items-center gap-2">
            <input
              type="text"
              readOnly
              value={form.bedrock_external_id}
              aria-label="External ID"
              onFocus={(e) => e.target.select()}
              className={`${glassInputClass} font-mono text-ui`}
            />
            <CopyButton value={form.bedrock_external_id} label="External ID" />
          </div>
        ) : (
          <p className="text-reported leading-relaxed text-ink-3">
            Generated when you run the setup below — add it to the role&apos;s trust policy.
          </p>
        )}
      </div>
      {/* Setup action — fetches the trust policy + external ID from the control
          service. A 422 (no ambient AWS identity) surfaces its remediation text
          inline. */}
      <div className="space-y-2">
        <button
          type="button"
          onClick={() => void runSetup()}
          disabled={loading}
          className="inline-flex items-center gap-1.5 rounded-xl border border-warm/20 px-4 py-2 text-body text-warm transition-colors hover:border-warm/30 hover:text-warm/80 disabled:opacity-40"
        >
          {loading
            ? 'Fetching…'
            : trustPolicy
              ? 'Refresh setup / trust policy'
              : 'Generate setup / show trust policy'}
        </button>
        {setupError && (
          <p className="text-reported leading-relaxed text-[var(--color-alarm)]">{setupError}</p>
        )}
        {callerArn && (
          <p className="text-reported leading-relaxed text-ink-3">
            Triage Factory assumes the role from{' '}
            <span className="font-mono text-ink-2">{callerArn}</span> — this identity must
            be allowed by the trust policy.
          </p>
        )}
        {trustPolicy && (
          <div className="space-y-1.5">
            <div className="flex items-center justify-between gap-2">
              <span className="text-reported font-medium uppercase tracking-wide text-ink-3">
                Trust policy
              </span>
              <CopyButton value={trustPolicy} label="trust policy" />
            </div>
            <pre className="max-h-64 overflow-auto rounded-lg border border-line-1 bg-tint-2 p-3 font-mono text-reported leading-relaxed text-ink-2">
              {trustPolicy}
            </pre>
            <p className="text-reported leading-relaxed text-ink-3">
              Create the role in IAM with Bedrock permissions, then set this as its trust policy.
            </p>
          </div>
        )}
      </div>
    </div>
  )
}

// BedrockFields is the Amazon Bedrock credential group: the auth-method
// picker (IAM role vs Bedrock API key vs IAM access keys), the selected
// method's inputs, and the shared non-secret config (region, optional model ID,
// optional endpoint override for VPC endpoints / GovCloud). `hasStored` is
// true when a credential for the CURRENTLY SELECTED method is already in the
// vault — it arms the "leave blank to keep current" masking; switching
// methods always requires the new method's secrets. Shared by the wizard key
// step and the Settings section.
export function BedrockFields({
  form,
  onChange,
  hasStored,
  invalid,
}: {
  form: OrgConfigForm
  onChange: (patch: Partial<OrgConfigForm>) => void
  hasStored: boolean
  invalid?: boolean
}) {
  const keep = hasStored ? ' (leave blank to keep current)' : ''
  return (
    <div className="space-y-4">
      <ChoiceCards
        ariaLabel="Bedrock authentication method"
        options={BEDROCK_AUTH_OPTIONS}
        selected={form.bedrock_auth_method}
        onChoose={(kind: BedrockAuthMethod) => onChange({ bedrock_auth_method: kind })}
      />
      {form.bedrock_auth_method === 'role' ? (
        <BedrockRoleFields form={form} onChange={onChange} invalid={invalid} />
      ) : form.bedrock_auth_method === 'bearer' ? (
        <LabeledInput
          label={`Bedrock API key${keep}`}
          value={form.bedrock_bearer_token}
          onChange={(v) => onChange({ bedrock_bearer_token: v })}
          placeholder={hasStored ? '••••••••' : 'bedrock-api-key-…'}
          secret
          invalid={invalid}
        />
      ) : (
        <div className="space-y-3">
          <LabeledInput
            label={`Access key ID${keep}`}
            value={form.aws_access_key_id}
            onChange={(v) => onChange({ aws_access_key_id: v })}
            placeholder={hasStored ? '••••••••' : 'AKIA…'}
            secret
            invalid={invalid}
          />
          <LabeledInput
            label={`Secret access key${keep}`}
            value={form.aws_secret_access_key}
            onChange={(v) => onChange({ aws_secret_access_key: v })}
            placeholder="••••••••"
            secret
            invalid={invalid}
          />
          <LabeledInput
            label="Session token (only for temporary credentials)"
            value={form.aws_session_token}
            onChange={(v) => onChange({ aws_session_token: v })}
            placeholder={hasStored ? '••••••••' : 'FwoG…'}
            secret
          />
        </div>
      )}
      <div className="grid gap-3 sm:grid-cols-2">
        <LabeledInput
          label="AWS region"
          value={form.bedrock_region}
          onChange={(v) => onChange({ bedrock_region: v })}
          placeholder="us-east-1"
          invalid={invalid && form.bedrock_region.trim() === ''}
        />
        <LabeledInput
          label="Model ID (optional)"
          value={form.bedrock_model_id}
          onChange={(v) => onChange({ bedrock_model_id: v })}
          placeholder="us.anthropic.claude-…"
        />
      </div>
      <div className="space-y-2">
        <LabeledInput
          label="Endpoint override (optional)"
          value={form.bedrock_base_url}
          onChange={(v) => onChange({ bedrock_base_url: v })}
          placeholder="https://bedrock-runtime.us-east-1.amazonaws.com"
        />
        <p className="text-reported leading-relaxed text-ink-3">
          Only needed for a VPC interface endpoint, GovCloud, or another endpoint the standard
          regional address can&apos;t reach. The region above still names the request-signing scope.
        </p>
      </div>
    </div>
  )
}

// OrgClaudeKeyStep body — the provider radio + the selected provider's
// credential fields. The connect is the step's Continue (steps.tsx drives
// connectAnthropic / connectBedrock and patches the connected flags on
// success), so there's no Connect button here. `error` reddens the fields on
// a failed validation.
export function OrgClaudeKeyStep({ state, patch, error }: StepContext) {
  return (
    <div className="space-y-5">
      <div className="space-y-1.5">
        <h2 className="text-[19px] font-medium tracking-tight text-ink-1">Connect Claude</h2>
        <p className="text-body leading-relaxed text-ink-3">
          The credentials Triage Factory uses to run Claude Code for delegated work.
        </p>
      </div>
      <ClaudeProviderCards
        selected={state.claudeProvider}
        onSelect={(kind) => patch({ claudeProvider: kind })}
      />
      {state.claudeProvider === 'anthropic' ? (
        <AnthropicKeyField
          value={state.org.anthropic_api_key}
          onChange={(v) => patch({ org: { ...state.org, anthropic_api_key: v } })}
          hasKey={state.anthropicConnected}
          invalid={!!error}
        />
      ) : (
        <BedrockFields
          form={state.org}
          onChange={(p) => patch({ org: { ...state.org, ...p } })}
          hasStored={
            state.bedrockConnected && state.bedrockStoredMethod === state.org.bedrock_auth_method
          }
          invalid={!!error}
        />
      )}
    </div>
  )
}
