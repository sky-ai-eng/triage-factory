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

  it('labels the gh-channel verbs, including the unclassified fallback row', () => {
    expect(metaForAction('pr_merged').label).toBe('PR merged')
    expect(metaForAction('reaction_added').label).toBe('Reaction added')
    expect(metaForAction('workflow_dispatched').label).toBe('Workflow dispatched')
    // The opaque row is the one the incident showed rendering as an anonymous
    // "action" — it still has no verb, but it must at least say what it is.
    expect(metaForAction('gh_channel_write')).not.toBe(FALLBACK_ACTION_META)
  })

  it('labels what arrives over GraphQL, verbs and fallback alike', () => {
    // Most of gh's porcelain writes are GraphQL, so these are not an exotic
    // corner: an unlabelled graphql_write would leave the commonest unnamed
    // write rendering as an anonymous action, which is the bug this row exists
    // to close.
    expect(metaForAction('pr_reopened').label).toBe('PR reopened')
    expect(metaForAction('graphql_write')).not.toBe(FALLBACK_ACTION_META)
    expect(metaForAction('graphql_write')).not.toBe(metaForAction('gh_channel_write'))
  })

  it('keeps arming a merge visually apart from performing one', () => {
    // The backend records these as different acts because they are: enabling
    // auto-merge merges nothing, and what lands later lands with no agent
    // present. A shared label here would undo that distinction at the only
    // place a person actually reads it.
    expect(metaForAction('pr_auto_merge_enabled').label).toBe('Auto-merge enabled')
    expect(metaForAction('pr_auto_merge_enabled')).not.toBe(metaForAction('pr_merged'))
    expect(metaForAction('pr_auto_merge_disabled')).not.toBe(metaForAction('pr_auto_merge_enabled'))
  })

  it('names every verb the gh-channel coverage sweep added', () => {
    for (const action of [
      'pr_reverted',
      'pr_branch_updated',
      'label_added',
      'label_removed',
      'conversation_locked',
      'conversation_unlocked',
      'issue_pinned',
      'issue_unpinned',
      'issue_transferred',
      'linked_branch_created',
    ]) {
      expect(metaForAction(action)).not.toBe(FALLBACK_ACTION_META)
    }
  })

  it('surfaces slack in the Actions-lens provider filter', () => {
    expect(ACTION_PROVIDERS).toContain('slack')
  })
})
