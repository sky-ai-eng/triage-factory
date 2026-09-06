# Accounts

The integration identities a person holds in a multi-mode org: which GitHub
account the factory matches their pull requests and reviews to, and which Jira
account it acts as. One line per system, a verb on the line, and a band under
the line when the verb is pressed. Decided on the design bundle's Accounts
canvas; the turns that matter there are 2, 3, 7, 8 and 10.

## What it is not

**Sign-in is not here.** A login identity (`auth.identities`, read by
`GET /api/me/identities`) is linked to the principal by verified email at
sign-in and has no member-side verb: no unlink, no re-link, no "change Entra
account" — a different Entra account is a different email, so a different
person. GoTrue never merges an SSO login with a GitHub one. That is a fact about
the session, and the **page header** states it — provider mark, provider, who,
and a faint "also signs in with …" line when two logins are linked (turn 9b).
Putting SSO in this list was tried three times and each time it was a row with
nothing to press. Do not add it back.

**GitHub appears in both places and means two things.** The header's GitHub is
the door; this list's GitHub is `user_github_identities`, keyed per host. On a
github.com org they are usually the same handle, because a GitHub login mirrors
itself into the binding (`source='login_claim'`); on a GHES org they are visibly
different accounts. The list is only ever the second one.

## The verb rule

- **Connect**, warm — the binding is absent. The only warm thing in the section:
  absence is the ask, and an unbound GitHub means the factory cannot tell which
  pull requests are yours.
- **Change**, quiet — GitHub is bound. Offered whatever wrote the binding,
  including the login mirror: the table is keyed `(user_id, github_base_url)` and
  Connect upserts into it, so a reader may make a different account theirs.
  The verb holds because the login mirror refreshes only rows the mirror
  wrote — a `pat` / `connect_oauth` binding survives the next sign-in.
- **Reconnect**, quiet — Jira is bound. Different word because Jira's credential
  is _stored_ so the factory can act as you, and a stored credential can lapse.
- **No verb** — `interactive={false}`, or offline. Absent, never disabled.

## The band

Pressing the verb opens a band under the line: the tokens table's draft row —
warm wash (`--color-warm-1`), a 2px spine, a warm rule on its foot, one sweep
across it as it opens. It is the same act as a draft row, a line being
rewritten, so it is the same flash. The verb becomes Cancel where it stood;
Escape does the same.

The body follows the org's access method for that system, never the reader's
choice:

| `method`     | body                                                                                                                        |
| ------------ | --------------------------------------------------------------------------------------------------------------------------- |
| github `app` | Continue with GitHub (redirect to the org's host, back with `connect_oauth`) · _or paste a personal access token_ as a link |
| github `pat` | the token field, open at once                                                                                               |
| jira `oauth` | Continue with Atlassian · _or use an API token_                                                                             |
| jira `cloud` | Atlassian account email + API token                                                                                         |
| jira `dc`    | one personal access token                                                                                                   |

The token path is a link under a Connect, not a second button: it exists for the
reader whose browser cannot complete the redirect, not as an equal choice.

A refusal is the server's own words, in mono, under the field, and the field
keeps its contents. A success collapses the band and the value line changes;
the change is marked once (`ac-tick`, keyed by the value, the rail's count-tick
on a line of text) and then it is over. No toast.

## Keyboard and semantics

Every verb is a `role="button"` with a tab stop; Enter and Space press it. The
band's Verify button is a real button, disabled until its fields are non-empty
(the one place "disabled" is right: the verb exists, its precondition does
not). Escape closes an open band, and so does Cancel — neither sends. Focus
rings draw for the keyboard only (`:focus-visible`), in the system's warm.

## Loading and failure

`loading` draws two skeleton rows — a hairline disc and three outlined bars at
the row's real proportions — so the section holds its height and the reader
sees its shape before its values. `offline` prints `--` in every value and
removes the verbs: a stale account is worse than no account, and a verb that
would 502 is not information.

## Do not

- Add sign-in, SSO, or a "login methods" list here. Header.
- Add a sentence under each line explaining what GitHub or Jira is for. Anyone
  on this page is already using the product (turn 7).
- Center the name on a two-line entry. The entries are one line; if a second
  line comes back, the name sits on the first baseline.
- Show a verb the reader cannot use. `interactive={false}` removes them.
- Use this for the setup wizard's User step. That is a separate design pass;
  the wizard's step contract is not this component's.

## Reduced motion

The sweep and the tick are CSS animations, so `tokens/motion.css` covers them;
this stylesheet also names its answer: the sweep is removed outright (it was
attention, not information) and the tick simply does not play — the new value
is already on the line.
