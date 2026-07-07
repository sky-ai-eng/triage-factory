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

import { glassInputClass } from '../settings/primitives'
import { CLAUDE_SOURCE_OPTIONS } from '../settings/anthropicConnect'
import { BEDROCK_AUTH_OPTIONS } from '../settings/bedrockConnect'
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
        <h2 className="text-[19px] font-medium tracking-tight text-text-primary">
          How should runs reach Claude?
        </h2>
        <p className="text-[13px] leading-relaxed text-text-tertiary">
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
                ? 'border-accent/50 bg-accent/[0.06] shadow-sm shadow-black/[0.03]'
                : 'border-border-subtle bg-black/[0.02] hover:border-border-strong'
            }`}
          >
            <span
              className={`text-[13px] font-medium ${
                isSelected ? 'text-accent' : 'text-text-primary'
              }`}
            >
              {card.title}
            </span>
            <span className="text-[11px] leading-snug text-text-tertiary">{card.blurb}</span>
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
      <span className="mb-2 block text-[11px] font-medium uppercase tracking-wide text-text-tertiary">
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
            ? ' !border-[var(--color-dismiss)] focus:!border-[var(--color-dismiss)] focus:!shadow-[0_0_0_4px_rgba(168,69,69,0.16)]'
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
      <p className="text-[11px] leading-relaxed text-text-tertiary">
        Create one in the{' '}
        <a
          href="https://console.anthropic.com/settings/keys"
          target="_blank"
          rel="noopener noreferrer"
          className="text-accent hover:underline"
        >
          Anthropic Console
        </a>
        . We check it with Anthropic before saving.
      </p>
    </div>
  )
}

// BedrockFields is the Amazon Bedrock credential group: the auth-method
// picker (Bedrock API key vs IAM access keys), the selected method's secret
// inputs, and the shared non-secret config (region, optional model ID,
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
      {form.bedrock_auth_method === 'bearer' ? (
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
        <p className="text-[11px] leading-relaxed text-text-tertiary">
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
        <h2 className="text-[19px] font-medium tracking-tight text-text-primary">Connect Claude</h2>
        <p className="text-[13px] leading-relaxed text-text-tertiary">
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
