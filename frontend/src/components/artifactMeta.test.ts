import { describe, it, expect } from 'vitest'
import { MessageCircle } from 'lucide-react'
import {
  metaForKind,
  FALLBACK_META,
  ARTIFACT_KINDS,
  ARTIFACT_PROVIDERS,
  PROVIDER_LABEL,
} from './artifactMeta'

describe('artifactMeta', () => {
  it('resolves the Slack message kind to its own meta, not the fallback', () => {
    const meta = metaForKind('message')
    expect(meta).not.toBe(FALLBACK_META)
    expect(meta.label).toBe('message')
    expect(meta.icon).toBe(MessageCircle)
  })

  it('still falls back for a kind outside the modeled union', () => {
    expect(metaForKind('holographic_shard')).toBe(FALLBACK_META)
  })

  it('surfaces message in the kind filter and slack in the provider filter', () => {
    expect(ARTIFACT_KINDS).toContain('message')
    expect(ARTIFACT_PROVIDERS).toContain('slack')
    expect(PROVIDER_LABEL['slack']).toBe('Slack')
  })
})
