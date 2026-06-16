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
//   OrgClaudeKeyStep    — the provider + key step: an Anthropic radio (selected)
//                         with Bedrock rendered disabled / "Soon", then the
//                         API-key paste field. Continue validates the key
//                         server-side (connectAnthropic) and blocks on failure.
//
// Reuse rule: composes the shared ChoiceCards picker and the wizard's glass
// field primitives — no parallel field UIs. The ClaudeProviderCards +
// AnthropicKeyField pieces are exported so the Settings section renders the same
// provider radio + key input.

import { Clock } from 'lucide-react'
import { glassInputClass } from '../settings/primitives'
import { CLAUDE_SOURCE_OPTIONS } from '../settings/anthropicConnect'
import { ChoiceCards } from './parts'
import type { StepContext } from './types'

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
          or bring your own Anthropic API key.
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
  kind: 'anthropic' | 'bedrock'
  title: string
  blurb: string
  disabled?: boolean
}

const PROVIDER_CARDS: ProviderCard[] = [
  { kind: 'anthropic', title: 'Anthropic API', blurb: 'A direct Anthropic API key (sk-ant-…).' },
  { kind: 'bedrock', title: 'Amazon Bedrock', blurb: 'Coming soon.', disabled: true },
]

// ClaudeProviderCards is the provider radio: Anthropic (the only selectable
// provider this release) and Bedrock as a disabled "coming soon" placeholder —
// the same treatment Linear gets in the Trackers step. Provider isn't stored
// (Anthropic is implied), so this is presentational, fixing Anthropic selected.
// Shared by the wizard key step and the Settings section.
export function ClaudeProviderCards() {
  return (
    <div role="group" aria-label="Claude provider" className="grid gap-2 sm:grid-cols-2">
      {PROVIDER_CARDS.map((card) => {
        const selected = card.kind === 'anthropic'
        return (
          <div
            key={card.kind}
            aria-current={selected ? 'true' : undefined}
            aria-disabled={card.disabled}
            className={`flex flex-col items-start gap-1 rounded-xl border px-3.5 py-3 text-left ${
              card.disabled
                ? 'border-border-subtle bg-black/[0.02] opacity-55'
                : 'border-accent/50 bg-accent/[0.06] shadow-sm shadow-black/[0.03]'
            }`}
          >
            <span className="flex items-center gap-1.5">
              <span
                className={`text-[13px] font-medium ${
                  selected ? 'text-accent' : 'text-text-primary'
                }`}
              >
                {card.title}
              </span>
              {card.disabled && (
                <span className="inline-flex items-center gap-0.5 rounded-full bg-black/[0.05] px-1.5 py-0.5 text-[9px] font-medium uppercase tracking-wide text-text-tertiary">
                  <Clock size={9} aria-hidden />
                  Soon
                </span>
              )}
            </span>
            <span className="text-[11px] leading-snug text-text-tertiary">{card.blurb}</span>
          </div>
        )
      })}
    </div>
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
      <label className="block">
        <span className="mb-2 block text-[11px] font-medium uppercase tracking-wide text-text-tertiary">
          Anthropic API key{hasKey ? ' (leave blank to keep current)' : ''}
        </span>
        <input
          type="password"
          value={value}
          placeholder={hasKey ? '••••••••' : 'sk-ant-…'}
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

// OrgClaudeKeyStep body — the provider radio + key field. The connect is the
// step's Continue (steps.tsx calls connectAnthropic and patches anthropicConnected
// on success), so there's no Connect button here. `error` reddens the field on a
// failed validation.
export function OrgClaudeKeyStep({ state, patch, error }: StepContext) {
  return (
    <div className="space-y-5">
      <div className="space-y-1.5">
        <h2 className="text-[19px] font-medium tracking-tight text-text-primary">Connect Claude</h2>
        <p className="text-[13px] leading-relaxed text-text-tertiary">
          The API key Triage Factory uses to run Claude Code for delegated work.
        </p>
      </div>
      <ClaudeProviderCards />
      <AnthropicKeyField
        value={state.org.anthropic_api_key}
        onChange={(v) => patch({ org: { ...state.org, anthropic_api_key: v } })}
        hasKey={state.anthropicConnected}
        invalid={!!error}
      />
    </div>
  )
}
