import { describe, it, expect } from 'vitest'
import {
  actionDetailSummary,
  ACTION_OPTIONS,
  ACTION_PROVIDERS,
  FALLBACK_ACTION_META,
  metaForAction,
} from './actionMeta'

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

  it('names the repository-configuration verbs', () => {
    for (const action of [
      'repo_created',
      'repo_edited',
      'repo_deleted',
      'repo_forked',
      'repo_archived',
      'repo_unarchived',
      'release_created',
      'release_edited',
      'release_deleted',
      'label_defined',
      'label_definition_edited',
      'label_definition_deleted',
    ]) {
      expect(metaForAction(action)).not.toBe(FALLBACK_ACTION_META)
    }
  })

  it('keeps a label definition apart from a label on an issue', () => {
    // Deleting the definition strips that label from every issue carrying it,
    // while label_removed takes one label off one issue. The two read alike at a
    // glance, so the labels have to do the separating.
    expect(metaForAction('label_definition_deleted').label).toBe('Label definition deleted')
    expect(metaForAction('label_definition_deleted')).not.toBe(metaForAction('label_removed'))
    expect(metaForAction('label_defined')).not.toBe(metaForAction('label_added'))
  })

  it('tells clearing a submitted review apart from the review lifecycle around it', () => {
    // The one review verb that overrides a human: the others move a review
    // through its own states, a dismissal revokes the standing of one somebody
    // already left. Falling back here would render it as an unnamed act, which
    // is the wrong way for that to appear in an audit lens.
    expect(metaForAction('review_dismissed').label).toBe('Review dismissed')
    expect(metaForAction('review_dismissed').tone).toBe('problem')
    expect(metaForAction('review_dismissed')).not.toBe(metaForAction('review_request_removed'))
  })

  it('surfaces slack in the Actions-lens provider filter', () => {
    expect(ACTION_PROVIDERS).toContain('slack')
  })

  it('names the refusals and the failed push, and lets them be filtered for', () => {
    // The security-signal rows: the three gates that turn something down, plus
    // the push that never landed. They were the last four rendering as an
    // anonymous "action" pill — and, because the filter dropdown is derived
    // from ACTION_META, the only rows an operator could not narrow the feed to.
    const denials = ['git_denied', 'egress_denied', 'branch_push_failed', 'gh_write_denied']
    for (const action of denials) {
      expect(metaForAction(action)).not.toBe(FALLBACK_ACTION_META)
      expect(ACTION_OPTIONS.map((o) => o.value)).toContain(action)
    }
    // Each names the boundary that refused it, since what to do about a
    // repo/ref denial and a blocked host are different things.
    expect(new Set(denials.map((a) => metaForAction(a).label)).size).toBe(denials.length)
  })

  it('routes sandbox egress denials through a provider the filter offers', () => {
    // egress_denied is recorded under provider 'network' and credential 'none' —
    // nothing left the box — so a github/jira/slack-only filter could never
    // isolate the rows an operator most wants to isolate.
    expect(ACTION_PROVIDERS).toContain('network')
  })

  it('still falls back for an action outside the modeled set, so a new one is never lost', () => {
    // Forward-compat is the point of the fallback: a backend that ships a verb
    // before this map learns it renders a row, not a blank.
    expect(metaForAction('repo_transferred_out')).toBe(FALLBACK_ACTION_META)
  })

  describe('actionDetailSummary', () => {
    it('spells the unclassified write as the request it was', () => {
      // The row this ticket exists for: gh_channel_write says only "a write
      // happened" until its method and path are on screen.
      expect(
        actionDetailSummary({
          target: 'acme/widgets',
          details: {
            method: 'POST',
            path: '/repos/acme/widgets/pulls/7/comments/12/replies',
            http_status: 201,
          },
        }),
      ).toBe('POST /repos/acme/widgets/pulls/7/comments/12/replies · http_status 201')
    })

    it('names what a refused write was reaching for', () => {
      expect(
        actionDetailSummary({
          target: 'acme/widgets',
          details: {
            method: 'PUT',
            path: '/repos/acme/widgets/pulls/7/merge',
            http_status: 404,
            attempted: 'pr_merged',
          },
        }),
      ).toContain('attempted pr_merged')
    })

    it('carries a denial reason', () => {
      expect(
        actionDetailSummary({
          target: 'acme/widgets',
          details: { op: 'receive-pack', reason: 'off_ref' },
        }),
      ).toBe('op receive-pack · reason off_ref')
    })

    it('renders a GraphQL row from whatever the envelope disclosed', () => {
      expect(
        actionDetailSummary({
          target: 'PR_kwDO',
          details: { operation: 'ClosePr', mutations: ['closePullRequest'], http_status: 200 },
        }),
      ).toBe('operation ClosePr · mutations closePullRequest · http_status 200')
    })

    it('drops what the row already says elsewhere', () => {
      // An egress denial repeats its host in detail_json, and a Jira transition
      // repeats its destination status — both already on the row.
      expect(
        actionDetailSummary({
          target: 'evil.example.com:443',
          details: { target: 'evil.example.com:443', reason: 'not_on_allowlist' },
        }),
      ).toBe('reason not_on_allowlist')
      expect(
        actionDetailSummary({
          target: 'SKY-1',
          from_state: 'To Do',
          to_state: 'In Progress',
          details: { status: 'In Progress' },
        }),
      ).toBeNull()
    })

    it('renders a true flag as its own name and drops a false one', () => {
      expect(actionDetailSummary({ details: { sha: 'abc123', new: true } })).toBe(
        'sha abc123 · new',
      )
      expect(actionDetailSummary({ details: { sha: 'abc123', new: false } })).toBe('sha abc123')
    })

    it('renders a key nobody anticipated rather than hiding it', () => {
      // The generic walk is deliberate: this epic keeps adding detail keys, and
      // a curated per-action list would leave each new one invisible — which is
      // the failure being fixed here.
      expect(actionDetailSummary({ details: { some_future_key: 'value' } })).toBe(
        'some_future_key value',
      )
    })

    it('has nothing to say when the row has no detail', () => {
      expect(actionDetailSummary({ details: undefined })).toBeNull()
      expect(actionDetailSummary({ details: {} })).toBeNull()
      // A non-object payload isn't a shape this can read; a row is still a row.
      expect(actionDetailSummary({ details: 'not an object' })).toBeNull()
      expect(actionDetailSummary({ details: [1, 2] })).toBeNull()
    })
  })
})
