// The org's commit signer: the GitHub user account agents' commits are
// authored, committed and signed as, so GitHub marks them Verified
// (docs/for-agents/specs/commit-signing). The section shows one of three
// states, read from GET commit-signer and GET commit-identity:
//
//   - no signer, and the org's runs commit unsigned as its GitHub identity;
//   - no signer, and the runs sign as the deployment's signer (managed
//     workspaces), which the identity read reports as `deployment`;
//   - the org's own signer, pending, active or broken.
//
// Every write answers with the signer as it now is. A write can also change
// who the org's runs commit as and how they sign, which only the identity read
// says, so the section re-reads it before showing the write's result.
//
// canManage follows the routes' gates: an org admin gets the controls, a
// member reads the same state without them.

import { useEffect, useId, useRef, useState } from 'react'
import { AlertTriangle, Check, Copy, ExternalLink } from 'lucide-react'
import { apiErrors, httpErrorMessage } from '../../lib/apiClient'
import {
  COMMIT_SIGNING_GUIDE_URL,
  createCommitSigner,
  deleteCommitSigner,
  getCommitIdentity,
  getCommitSigner,
  heldFor,
  isGitHubCom,
  isManagedUser,
  rotateCommitSigner,
  setCommitSignerEmail,
  verifyCommitSigner,
  type CommitIdentity,
  type CommitSigner,
} from '../../lib/commitSigner'
import { hostOf } from '../../lib/reachability'
import { inputClass } from './primitives'

const primaryBtn =
  'rounded-full bg-warm px-5 py-2 text-body font-medium text-warm-ink transition-colors hover:bg-warm/90 disabled:opacity-40'
const secondaryBtn =
  'rounded-xl border border-line-1 px-4 py-2 text-body font-medium text-ink-2 transition-colors hover:border-warm/40 hover:text-ink-1 disabled:opacity-40'
const dangerBtn =
  'rounded-xl border border-line-1 px-4 py-2 text-body font-medium text-ink-3 transition-colors hover:border-alarm/40 hover:text-alarm disabled:opacity-40'
const quietBtn =
  'rounded-xl px-3 py-2 text-body font-medium text-ink-3 transition-colors hover:text-ink-2 disabled:opacity-40'

interface Loaded {
  signer: CommitSigner | null
  identity: CommitIdentity
}

export default function CommitSigningGroup({
  orgId,
  githubUrl,
  canManage,
}: {
  orgId: string | null
  // The org's GitHub URL, '' when none is set. It decides whether a new
  // signer needs an address of its own.
  githubUrl: string
  canManage: boolean
}) {
  const [data, setData] = useState<Loaded | null>(null)
  const [loadError, setLoadError] = useState<string | null>(null)
  // Bumping reloadKey retries a failed load; the effect's cleanup drops a read
  // a newer one has overtaken.
  const [reloadKey, setReloadKey] = useState(0)
  const [settingUp, setSettingUp] = useState(false)

  useEffect(() => {
    if (!orgId) return
    let cancelled = false
    Promise.all([getCommitSigner(orgId), getCommitIdentity(orgId)])
      .then(([signer, identity]) => {
        if (!cancelled) setData({ signer, identity })
      })
      .catch((e) => {
        if (!cancelled) setLoadError(httpErrorMessage(e, 'Could not load commit signing.'))
      })
    return () => {
      cancelled = true
    }
  }, [orgId, reloadKey])

  // settle shows a write's answer with the identity it leaves. A failed
  // identity read keeps the last one: the write itself landed.
  const settle = async (signer: CommitSigner | null) => {
    if (!orgId) return
    const identity = await getCommitIdentity(orgId).catch(() => data?.identity)
    if (identity) setData({ signer, identity })
  }

  if (!orgId) return null
  if (!data) {
    if (loadError) {
      return (
        <div className="space-y-2">
          <p className="text-ui text-alarm">{loadError}</p>
          <button
            type="button"
            onClick={() => {
              setLoadError(null)
              setReloadKey((k) => k + 1)
            }}
            className={secondaryBtn}
          >
            Try again
          </button>
        </div>
      )
    }
    return <p className="text-ui text-ink-3">Loading commit signing…</p>
  }

  const { signer, identity } = data
  return (
    <div className="space-y-4">
      {signer ? (
        <SignerPanel
          orgId={orgId}
          signer={signer}
          identity={identity}
          canManage={canManage}
          onChange={settle}
        />
      ) : settingUp ? (
        <SetupForm
          orgId={orgId}
          githubUrl={githubUrl}
          onCancel={() => setSettingUp(false)}
          onCreated={async (created) => {
            await settle(created)
            setSettingUp(false)
          }}
        />
      ) : identity.signing === 'deployment' ? (
        <div className="space-y-3">
          <p className="text-body leading-relaxed text-ink-2">
            Commits are signed as{' '}
            <span className="font-medium text-ink-1">
              {identity.identity?.login ? `@${identity.identity.login}` : identity.identity?.name}
            </span>
            , this deployment&rsquo;s signing account.
          </p>
          {canManage && (
            <>
              <p className="text-body leading-relaxed text-ink-3">
                Register a GitHub account of your own to sign as it instead.
              </p>
              <button type="button" onClick={() => setSettingUp(true)} className={secondaryBtn}>
                Use your own account
              </button>
            </>
          )}
        </div>
      ) : (
        <div className="space-y-3">
          <p className="text-body leading-relaxed text-ink-2">
            {identity.identity ? (
              <>
                Commits are authored by{' '}
                <span className="font-medium text-ink-1">{identity.identity.name}</span> and
                aren&rsquo;t signed.
              </>
            ) : (
              <>Commits aren&rsquo;t signed.</>
            )}
          </p>
          <p className="text-body leading-relaxed text-ink-3">
            Register a GitHub user account as the signer, and agents&rsquo; commits are authored,
            committed and signed as it. GitHub then marks them Verified, and repositories that
            require signed commits accept them.
          </p>
          {canManage &&
            (githubUrl ? (
              <button type="button" onClick={() => setSettingUp(true)} className={secondaryBtn}>
                Set up signing
              </button>
            ) : (
              <p className="text-body leading-relaxed text-ink-3">
                Set the GitHub URL and connect GitHub above before setting up signing.
              </p>
            ))}
        </div>
      )}
      <a
        href={COMMIT_SIGNING_GUIDE_URL}
        target="_blank"
        rel="noreferrer"
        className="inline-flex items-center gap-1 text-ui text-warm hover:underline"
      >
        How to create the signer account
        <ExternalLink size={12} aria-hidden />
      </a>
    </div>
  )
}

// SetupForm registers the signer: a login, and on any host but github.com the
// address its commits carry, because only github.com has a noreply address to
// derive.
function SetupForm({
  orgId,
  githubUrl,
  onCancel,
  onCreated,
}: {
  orgId: string
  githubUrl: string
  onCancel: () => void
  onCreated: (s: CommitSigner) => Promise<void>
}) {
  const needsEmail = !isGitHubCom(githubUrl)
  const [login, setLogin] = useState('')
  const [email, setEmail] = useState('')
  const [busy, setBusy] = useState(false)
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({})
  const loginErrId = useId()
  const emailErrId = useId()
  const [error, setError] = useState<string | null>(null)

  const submit = async () => {
    setBusy(true)
    setError(null)
    setFieldErrors({})
    try {
      const created = await createCommitSigner(
        orgId,
        login.trim(),
        needsEmail ? email.trim() : undefined,
      )
      await onCreated(created)
    } catch (e) {
      // A fault on a field this form shows goes under that field; anything
      // else is the form's.
      const byField: Record<string, string> = {}
      const rest: string[] = []
      for (const item of apiErrors(e)) {
        if (item.field === 'login' || (item.field === 'email' && needsEmail)) {
          byField[item.field] = item.message
        } else {
          rest.push(item.message)
        }
      }
      setFieldErrors(byField)
      if (rest.length > 0) setError(rest.join(' '))
      else if (Object.keys(byField).length === 0)
        setError(httpErrorMessage(e, 'Could not register the signer.'))
    } finally {
      setBusy(false)
    }
  }

  const ready = login.trim() !== '' && (!needsEmail || email.trim() !== '')
  return (
    <form
      noValidate
      className="space-y-4"
      onSubmit={(e) => {
        e.preventDefault()
        if (ready && !busy) void submit()
      }}
    >
      <p className="text-body leading-relaxed text-ink-3">
        Name the GitHub user account to sign as. Triage Factory generates a signing key for it, and
        you add the public half to the account.
      </p>
      <div className="space-y-1.5">
        <label className="block space-y-1.5">
          <span className="block text-reported text-ink-3">GitHub login</span>
          <input
            type="text"
            value={login}
            onChange={(e) => setLogin(e.target.value)}
            placeholder="acme-agent"
            autoComplete="off"
            spellCheck={false}
            aria-invalid={!!fieldErrors.login}
            aria-describedby={fieldErrors.login ? loginErrId : undefined}
            className={inputClass}
          />
        </label>
        {fieldErrors.login && (
          <p id={loginErrId} className="text-ui text-alarm">
            {fieldErrors.login}
          </p>
        )}
      </div>
      {needsEmail && (
        <div className="space-y-1.5">
          <label className="block space-y-1.5">
            <span className="block text-reported text-ink-3">Commit email</span>
            <input
              type="email"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              placeholder="agent@example.com"
              autoComplete="off"
              spellCheck={false}
              aria-invalid={!!fieldErrors.email}
              aria-describedby={fieldErrors.email ? emailErrId : undefined}
              className={inputClass}
            />
          </label>
          <p className="text-reported leading-relaxed text-ink-3">
            An address verified on the account. {hostOf(githubUrl)} has no noreply address for
            commits to carry, so the signer needs one of its own.
          </p>
          {fieldErrors.email && (
            <p id={emailErrId} className="text-ui text-alarm">
              {fieldErrors.email}
            </p>
          )}
        </div>
      )}
      {error && <p className="text-ui text-alarm">{error}</p>}
      <div className="flex items-center gap-2">
        <button type="submit" disabled={!ready || busy} className={primaryBtn}>
          {busy ? 'Checking…' : 'Continue'}
        </button>
        <button type="button" onClick={onCancel} disabled={busy} className={quietBtn}>
          Cancel
        </button>
      </div>
    </form>
  )
}

type Action = 'verify' | 'rotate' | 'remove'

// SignerPanel is the org's own signer: its account, status, the address its
// commits carry, and the admin's controls.
function SignerPanel({
  orgId,
  signer,
  identity,
  canManage,
  onChange,
}: {
  orgId: string
  signer: CommitSigner
  identity: CommitIdentity
  canManage: boolean
  onChange: (s: CommitSigner | null) => Promise<void>
}) {
  const [busy, setBusy] = useState<Action | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [confirmingRemove, setConfirmingRemove] = useState(false)
  // Set when a Verify promoted a rotation's key, to say what to do about the
  // old public key. Session-only: the server keeps no record of the old key.
  const [rotated, setRotated] = useState(false)

  const act = async (
    action: Action,
    fallback: string,
    call: () => Promise<CommitSigner | null>,
  ) => {
    setBusy(action)
    setError(null)
    try {
      const next = await call()
      if (action === 'verify') setRotated(!!signer.pending_public_key && !next?.pending_public_key)
      if (action === 'rotate') setRotated(false)
      await onChange(next)
    } catch (e) {
      setError(httpErrorMessage(e, fallback))
    } finally {
      setBusy(null)
    }
  }

  const rotating = !!signer.pending_public_key
  const keyToAdd = signer.pending_public_key ?? signer.public_key
  const showSteps = signer.status === 'pending' || rotating
  return (
    <div className="space-y-4">
      <div className="space-y-1">
        <p className="text-body text-ink-2">
          Signer: <span className="font-medium text-ink-1">{signer.commit_name}</span>{' '}
          <span className="text-ink-3">
            (@{signer.login}
            {!isGitHubCom(signer.host) && ` on ${hostOf(signer.host)}`})
          </span>
        </p>
        <StatusLine signer={signer} />
      </div>

      <StatusDetail signer={signer} identity={identity} />

      {rotated && (
        <p className="text-body leading-relaxed text-ink-3">
          The new key now signs, and Triage Factory has destroyed the old key&rsquo;s private half,
          so the old key signs nothing. Leave the old public key on @{signer.login} unless it may
          have leaked: on a GitHub Enterprise Server with persistent commit verification turned off,
          deleting it turns the commits it signed Unverified.
        </p>
      )}

      {showSteps ? (
        <KeySteps signer={signer} keyToAdd={keyToAdd} rotating={rotating} canManage={canManage} />
      ) : (
        <CopyField label="Public key" value={signer.public_key} />
      )}

      <EmailRow orgId={orgId} signer={signer} canManage={canManage} onChange={onChange} />

      {canManage && (
        <div className="space-y-3">
          {error && <p className="text-ui text-alarm">{error}</p>}
          {confirmingRemove ? (
            <div className="space-y-3 rounded-xl border border-line-1 bg-tint-2 px-4 py-3">
              <p className="text-body leading-relaxed text-ink-2">
                Remove @{signer.login} as the signer? Triage Factory deletes its signing keys, and
                runs stop committing as the account. The public key stays on the account until you
                delete it there.
              </p>
              <div className="flex items-center gap-2">
                <button
                  type="button"
                  disabled={busy !== null}
                  onClick={() =>
                    void act('remove', 'Could not remove the signer.', async () => {
                      await deleteCommitSigner(orgId)
                      return null
                    })
                  }
                  className="rounded-xl border border-alarm/40 px-4 py-2 text-body font-medium text-alarm transition-colors hover:bg-alarm/[0.06] disabled:opacity-40"
                >
                  {busy === 'remove' ? 'Removing…' : 'Remove signer'}
                </button>
                <button
                  type="button"
                  disabled={busy !== null}
                  onClick={() => setConfirmingRemove(false)}
                  className={quietBtn}
                >
                  Keep it
                </button>
              </div>
            </div>
          ) : (
            <div className="flex flex-wrap items-center gap-2">
              <button
                type="button"
                disabled={busy !== null}
                onClick={() =>
                  void act('verify', 'Could not verify the signer.', () =>
                    verifyCommitSigner(orgId),
                  )
                }
                className={showSteps ? primaryBtn : secondaryBtn}
              >
                {busy === 'verify' ? 'Verifying…' : 'Verify'}
              </button>
              <button
                type="button"
                disabled={busy !== null}
                onClick={() =>
                  void act('rotate', 'Could not rotate the key.', () => rotateCommitSigner(orgId))
                }
                className={secondaryBtn}
              >
                {busy === 'rotate' ? 'Rotating…' : 'Rotate key'}
              </button>
              <button
                type="button"
                disabled={busy !== null}
                onClick={() => setConfirmingRemove(true)}
                className={dangerBtn}
              >
                Remove
              </button>
            </div>
          )}
        </div>
      )}
    </div>
  )
}

const statusLabel: Record<CommitSigner['status'], string> = {
  pending: 'Pending',
  active: 'Active',
  broken: 'Broken',
}

// StatusLine is the signer's status and how long it has held.
function StatusLine({ signer }: { signer: CommitSigner }) {
  const held = heldFor(signer.status_since)
  const tone =
    signer.status === 'broken'
      ? 'text-alarm'
      : signer.status === 'pending'
        ? 'text-warm'
        : 'text-ink-2'
  return (
    <p className="text-ui text-ink-3" title={new Date(signer.status_since).toLocaleString()}>
      <span className={`font-medium ${tone}`}>{statusLabel[signer.status]}</span>
      {held ? ` for ${held}` : ', as of just now'}
    </p>
  )
}

// StatusDetail says what the status means for the org's commits and, for a
// broken signer, why and what to do.
function StatusDetail({ signer, identity }: { signer: CommitSigner; identity: CommitIdentity }) {
  if (signer.status === 'broken') {
    return (
      <div
        role="alert"
        className="flex items-start gap-2.5 rounded-xl border border-alarm/25 bg-alarm/[0.06] px-3.5 py-3"
      >
        <AlertTriangle size={14} aria-hidden className="mt-0.5 shrink-0 text-alarm" />
        <div className="space-y-1">
          <p className="text-[13px] font-medium text-ink-2">{brokenReason(signer)}</p>
          <p className="text-[12px] leading-relaxed text-ink-3">
            Commits are still authored as {signer.commit_name}, but they aren&rsquo;t signed until
            Verify passes.
          </p>
        </div>
      </div>
    )
  }
  if (signer.status === 'pending') {
    return (
      <p className="text-body leading-relaxed text-ink-3">
        Commits are authored as {signer.commit_name}, but they aren&rsquo;t signed until Verify
        finds the key on the account.
      </p>
    )
  }
  if (identity.unsigned_reason) {
    return (
      <p className="text-body leading-relaxed text-ink-3">
        {unsignedReason(identity)} Commits are authored as {signer.commit_name} but aren&rsquo;t
        signed.
      </p>
    )
  }
  return (
    <p className="text-body leading-relaxed text-ink-3">
      Agents&rsquo; commits are signed as @{signer.login}.
    </p>
  )
}

// brokenReason puts the reason a signer broke in words: the ones an admin can
// fix on the account get what to do, and any other reason GitHub gives is
// shown as GitHub gave it.
function brokenReason(s: CommitSigner): string {
  switch (s.status_reason) {
    case 'unknown_key':
      return `GitHub reported that the key a pushed commit was signed with is not on @${s.login}. Add the public key below to the account as a signing key, then verify.`
    case 'no_user':
      return `GitHub reported that ${s.commit_email}, the address commits carry, belongs to no account. Add and verify it on @${s.login}, or change the commit email, then verify.`
    case 'unverified_email':
      return `${s.commit_email} is not verified on @${s.login}. Verify the address on the account, then verify here.`
    case 'key_missing':
      return `The signing key is no longer on @${s.login}. Add the public key below back to the account as a signing key, then verify.`
    case null:
      return 'A check of the signer failed.'
    default:
      return `GitHub reported a pushed commit as not verified: ${s.status_reason}.`
  }
}

// unsignedReason says why an active signer's commits still go unsigned: local
// mode signs with this machine's git.
function unsignedReason(identity: CommitIdentity): string {
  switch (identity.unsigned_reason) {
    case 'git_too_old':
      return `This machine’s git${identity.git_version ? `, ${identity.git_version},` : ''} is older than 2.34, the first version that signs with an SSH key.`
    case 'git_unavailable':
      return 'Triage Factory could not run git on this machine to check its version.'
    default:
      return `Signing is off: ${identity.unsigned_reason}.`
  }
}

// KeySteps is how to add a key to the account: the new signer's key, or a
// rotation's.
function KeySteps({
  signer,
  keyToAdd,
  rotating,
  canManage,
}: {
  signer: CommitSigner
  keyToAdd: string
  rotating: boolean
  canManage: boolean
}) {
  return (
    <div className="space-y-3">
      {rotating && (
        <p className="text-body leading-relaxed text-ink-3">
          A key rotation is under way. The current key keeps signing until Verify finds the new key
          on the account, and then the new key replaces it.
        </p>
      )}
      {canManage && (
        <ol className="list-decimal space-y-1.5 pl-5 text-body leading-relaxed text-ink-2">
          {isManagedUser(signer.host, signer.login) && (
            <li>
              Verify the account&rsquo;s email on GitHub first. @{signer.login} is a managed user,
              whose email can start unverified, and GitHub verifies no commit from it until the
              email is verified.
            </li>
          )}
          <li>
            Sign in to GitHub as @{signer.login} and open{' '}
            <a
              href={`${signer.host.replace(/\/+$/, '')}/settings/ssh/new`}
              target="_blank"
              rel="noreferrer"
              className="text-warm hover:underline"
            >
              Settings → SSH and GPG keys → New SSH key
            </a>
            .
          </li>
          <li>
            Set the title to <span className="font-medium">{signer.key_title}</span> and the key
            type to <span className="font-medium">Signing key</span>, paste the key below, and add
            it.
          </li>
          <li>Come back here and press Verify.</li>
        </ol>
      )}
      <CopyField label={rotating ? 'New public key' : 'Public key'} value={keyToAdd} />
    </div>
  )
}

// EmailRow is the address the signer's commits carry, and the admin's way to
// change it.
function EmailRow({
  orgId,
  signer,
  canManage,
  onChange,
}: {
  orgId: string
  signer: CommitSigner
  canManage: boolean
  onChange: (s: CommitSigner) => Promise<void>
}) {
  const [editing, setEditing] = useState(false)
  const [draft, setDraft] = useState('')
  const errId = useId()
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const derives = isGitHubCom(signer.host)

  const save = async (email: string | null) => {
    setBusy(true)
    setError(null)
    try {
      await onChange(await setCommitSignerEmail(orgId, email))
      setEditing(false)
    } catch (e) {
      setError(httpErrorMessage(e, 'Could not change the commit email.'))
    } finally {
      setBusy(false)
    }
  }

  if (!editing) {
    return (
      <div className="flex flex-wrap items-center justify-between gap-2">
        <p className="min-w-0 text-body text-ink-2">
          Commit email: <span className="font-medium text-ink-1">{signer.commit_email}</span>
          {signer.email === null && (
            <span className="text-ink-3"> (GitHub&rsquo;s noreply address)</span>
          )}
        </p>
        {canManage && (
          <button
            type="button"
            onClick={() => {
              setDraft(signer.email ?? '')
              setError(null)
              setEditing(true)
            }}
            className={quietBtn}
          >
            Change
          </button>
        )}
      </div>
    )
  }

  return (
    <form
      noValidate
      className="space-y-3"
      onSubmit={(e) => {
        e.preventDefault()
        if (draft.trim() && !busy) void save(draft.trim())
      }}
    >
      <label className="block space-y-1.5">
        <span className="block text-reported text-ink-3">Commit email</span>
        <input
          type="email"
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
          placeholder="agent@example.com"
          autoComplete="off"
          spellCheck={false}
          aria-describedby={error ? errId : undefined}
          className={inputClass}
        />
      </label>
      <p className="text-body leading-relaxed text-ink-3">
        Add the address to @{signer.login} on GitHub and verify it there first. GitHub marks a
        commit Verified only when its committer address is verified on the signing account, and
        Triage Factory cannot check the account&rsquo;s addresses before a commit is pushed.
      </p>
      {error && (
        <p id={errId} className="text-ui text-alarm">
          {error}
        </p>
      )}
      <div className="flex flex-wrap items-center gap-2">
        <button type="submit" disabled={!draft.trim() || busy} className={primaryBtn}>
          {busy ? 'Saving…' : 'Save'}
        </button>
        {derives && signer.email !== null && (
          <button
            type="button"
            disabled={busy}
            onClick={() => void save(null)}
            className={secondaryBtn}
          >
            Use the noreply address
          </button>
        )}
        <button
          type="button"
          disabled={busy}
          onClick={() => setEditing(false)}
          className={quietBtn}
        >
          Cancel
        </button>
      </div>
    </form>
  )
}

// CopyField is a read-only value with a Copy button, the settings idiom
// (SSOSettings, LinearOAuthAppCard).
function CopyField({ label, value }: { label: string; value: string }) {
  const [copied, setCopied] = useState(false)
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null)
  useEffect(
    () => () => {
      if (timerRef.current) clearTimeout(timerRef.current)
    },
    [],
  )
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(value)
      setCopied(true)
      if (timerRef.current) clearTimeout(timerRef.current)
      timerRef.current = setTimeout(() => setCopied(false), 1500)
    } catch {
      // Clipboard denied — the value is still selectable in the field.
    }
  }
  return (
    <div className="space-y-1.5">
      <span className="block text-reported text-ink-3">{label}</span>
      <div className="flex items-center gap-2">
        <input
          type="text"
          readOnly
          value={value}
          aria-label={label}
          onFocus={(e) => e.target.select()}
          className={`${inputClass} font-mono text-ui`}
        />
        <button
          type="button"
          onClick={() => void copy()}
          aria-label={`Copy ${label}`}
          className="inline-flex shrink-0 items-center gap-1 rounded-xl border border-line-1 px-3 py-2 text-ui font-medium text-ink-2 transition-colors hover:text-ink-1"
        >
          {copied ? <Check size={13} /> : <Copy size={13} />}
          {copied ? 'Copied' : 'Copy'}
        </button>
      </div>
    </div>
  )
}
