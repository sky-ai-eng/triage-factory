import { describe, it, expect } from 'vitest'
import { metaForAction, FALLBACK_ACTION_META, ACTION_PROVIDERS } from './actionMeta'

describe('actionMeta', () => {
  it('resolves each Slack action to its own meta, not the fallback', () => {
    expect(metaForAction('slack_message_posted').label).toBe('Message posted')
    expect(metaForAction('slack_message_edited').label).toBe('Message edited')
    expect(metaForAction('slack_reaction_added').label).toBe('Reaction added')
    for (const action of ['slack_message_posted', 'slack_message_edited', 'slack_reaction_added']) {
      expect(metaForAction(action)).not.toBe(FALLBACK_ACTION_META)
    }
  })

  it('still falls back for an action outside the modeled set', () => {
    expect(metaForAction('slack_channel_archived')).toBe(FALLBACK_ACTION_META)
  })

  it('surfaces slack in the Actions-lens provider filter', () => {
    expect(ACTION_PROVIDERS).toContain('slack')
  })
})
