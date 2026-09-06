import { useState } from 'react'
import { Accounts } from '../ui/accounts/Accounts'
import type { Account, AccountMethod } from '../ui/accounts/Accounts'

// The Accounts harness. The review is the band: press a verb and watch the
// body follow the ORG's access method, picked by the chips — never the
// reader's choice — then refuse a token that starts with "bad" in the server's
// words and land any other after a beat.

const verify = (id: string, { token, email }: { token: string; email?: string }) =>
  new Promise<string>((ok, no) =>
    setTimeout(() => {
      if (/^bad/i.test(token))
        return no(
          new Error(
            id === 'gh'
              ? '401 from github.com — the token was refused'
              : '401 from sky-ai.atlassian.net — email or token refused',
          ),
        )
      ok(
        id === 'gh'
          ? '@' + (token.replace(/^ghp_/, '').slice(0, 8) || 'aallchin')
          : email || 'aidan@allchin.com',
      )
    }, 900),
  )

export function AccountsCard() {
  const [ghMethod, setGhMethod] = useState<AccountMethod>('app')
  const [jiraMethod, setJiraMethod] = useState<AccountMethod>('oauth')
  const [log, setLog] = useState('nothing sent yet')
  const bound: Account[] = [
    { id: 'gh', kind: 'github', account: '@aallchin', host: 'github.com', method: ghMethod },
    {
      id: 'jira',
      kind: 'jira',
      account: 'aidan@allchin.com',
      host: 'sky-ai.atlassian.net',
      method: jiraMethod,
    },
  ]
  const unbound = bound.map((a) => ({ ...a, account: null }))
  const chip = (on: boolean, label: string, pick: () => void) => (
    <button type="button" className="gal-chip" data-on={on ? '' : undefined} onClick={pick}>
      {label}
    </button>
  )

  return (
    <div className="gal-card">
      <div className="gal-cardhead">
        <span>Accounts</span>
        <span className="gal-route">{log}</span>
      </div>

      <p className="gal-note">
        The integration identities a person holds — GitHub and Jira — one line per system with a
        verb on the line. <strong>Change</strong> and <strong>Reconnect</strong> open a band under
        the entry whose body is the org&rsquo;s access method, picked below. A token starting with{' '}
        <code>bad</code> is refused in the server&rsquo;s words and the field keeps what you pasted;
        anything else lands after a beat, the band closes and the value ticks once. <kbd>Esc</kbd>{' '}
        and Cancel close without sending. Sign-in never appears here — it is a header fact.
      </p>

      <div className="gal-chips">
        {chip(ghMethod === 'app', 'github: org App', () => setGhMethod('app'))}
        {chip(ghMethod === 'pat', 'github: PAT', () => setGhMethod('pat'))}
        {chip(jiraMethod === 'oauth', 'jira: OAuth app', () => setJiraMethod('oauth'))}
        {chip(jiraMethod === 'cloud', 'jira: Cloud', () => setJiraMethod('cloud'))}
        {chip(jiraMethod === 'dc', 'jira: Data Center', () => setJiraMethod('dc'))}
      </div>

      <div className="gal-specimens">
        <div className="gal-spec">
          <span className="gal-spec-tag">bound · press a verb</span>
          <Accounts
            accounts={bound}
            onConnect={(id) =>
              setLog(
                'redirect → ' +
                  (id === 'gh'
                    ? '/api/orgs/{org}/github/connect/start'
                    : '/api/orgs/{org}/jira/connect/start'),
              )
            }
            onVerify={verify}
            onChange={(id, account) => setLog('bound ' + id + ' → ' + account)}
          />
        </div>
        <div className="gal-spec">
          <span className="gal-spec-tag">nothing bound — Connect is the only warm verb</span>
          <Accounts accounts={unbound} onConnect={() => setLog('redirect')} onVerify={verify} />
        </div>
        <div className="gal-spec">
          <span className="gal-spec-tag">a readout · loading · offline</span>
          <Accounts accounts={bound} interactive={false} note="a readout — no verbs" />
          <Accounts accounts={[]} loading />
          <Accounts accounts={bound} offline />
        </div>
      </div>
    </div>
  )
}

export default AccountsCard
