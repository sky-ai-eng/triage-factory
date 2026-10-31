import { describe, it, expect } from 'vitest'
import { MessageCircle } from 'lucide-react'
import { metaForKind, FALLBACK_META, PROVIDER_LABEL } from './deliverableMeta'

describe('deliverableMeta', () => {
  it('resolves the Slack message kind to its own meta, not the fallback', () => {
    const meta = metaForKind('message')
    expect(meta).not.toBe(FALLBACK_META)
    expect(meta.label).toBe('message')
    expect(meta.icon).toBe(MessageCircle)
  })

  it('still falls back for a kind outside the modeled union', () => {
    expect(metaForKind('holographic_shard')).toBe(FALLBACK_META)
  })

  it('labels slack for the provider filters', () => {
    expect(PROVIDER_LABEL['slack']).toBe('Slack')
  })
})
