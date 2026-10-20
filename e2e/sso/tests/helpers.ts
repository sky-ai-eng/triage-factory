import { execFileSync } from 'node:child_process'
import { readFileSync } from 'node:fs'
import path from 'node:path'
import {
  expect,
  request,
  type APIRequestContext,
  type Browser,
  type BrowserContext,
  type Page,
} from '@playwright/test'

export const BASE = 'http://localhost:3000'
export const GLOBEX_METADATA = 'https://idp-globex.test:8443/metadata'
export const MALLORY_METADATA = 'https://idp-mallory.test:8444/metadata'

const STATE = process.env.SSO_E2E_STATE ?? path.resolve(__dirname, '..', '.state')

type IdPUser = { user: string; password: string; email: string }
const fixtures = JSON.parse(
  readFileSync(path.resolve(__dirname, '..', 'fixtures', 'users.json'), 'utf8'),
) as {
  idps: Record<'globex' | 'mallory', IdPUser[]>
}

export function idpUser(idp: 'globex' | 'mallory', user: string): IdPUser {
  const u = fixtures.idps[idp].find((x) => x.user === user)
  if (!u) throw new Error(`no ${idp} IdP user ${user} in users.json`)
  return u
}

/** A signed-in browser context: its page and an API client carrying its cookies. */
export type Session = { context: BrowserContext; page: Page; api: APIRequestContext }

async function newSession(browser: Browser): Promise<Session> {
  const context = await browser.newContext({ baseURL: BASE, ignoreHTTPSErrors: true })
  const page = await context.newPage()
  return { context, page, api: context.request }
}

/** Signs in through the fake GitHub from the login page. */
export async function signInGitHub(browser: Browser, login: string): Promise<Session> {
  const s = await newSession(browser)
  await s.page.goto('/login')
  await s.page
    .getByRole('button', { name: 'Sign in with GitHub' })
    .or(s.page.getByRole('link', { name: 'Sign in with GitHub' }))
    .click()
  await s.page.waitForURL(/github\.test:8445\/login\/oauth\/authorize/)
  await s.page.getByLabel('Username').fill(login)
  await s.page.getByRole('button', { name: 'Authorize' }).click()
  await s.page.waitForURL(
    (u) => u.host === 'localhost:3000' && !u.pathname.startsWith('/api/auth/'),
  )
  return s
}

/** Fills a crewjam IdP's login form, then waits for TF to finish the callback. */
async function completeIdPLogin(page: Page, u: IdPUser) {
  await page.waitForURL(/idp-(globex|mallory)\.test/)
  await page.locator('input[name="user"]').fill(u.user)
  await page.locator('input[name="password"]').fill(u.password)
  await page.locator('input[type="submit"]').click()
}

/** Identifier-first sign-in: types the email on /login and follows discovery to the IdP. */
export async function signInByEmail(browser: Browser, u: IdPUser): Promise<Session> {
  const s = await newSession(browser)
  await s.page.goto('/login')
  await s.page.getByLabel('Work email').fill(u.email)
  await s.page.getByRole('button', { name: 'Continue' }).click()
  await completeIdPLogin(s.page, u)
  await s.page.waitForURL(
    (url) => url.host === 'localhost:3000' && !url.pathname.startsWith('/api/auth/'),
  )
  return s
}

/** Signs in through one connection by its provider id, as an IdP's app tile would. */
export async function signInThroughProvider(
  browser: Browser,
  providerID: string,
  u: IdPUser,
): Promise<Session> {
  const s = await newSession(browser)
  await s.page.goto(
    `/api/auth/oauth/saml?provider_id=${encodeURIComponent(providerID)}&return_to=%2F`,
  )
  await completeIdPLogin(s.page, u)
  await s.page.waitForURL(
    (url) => url.host === 'localhost:3000' && !url.pathname.startsWith('/api/auth/'),
  )
  return s
}

export type MeOrg = {
  id: string
  name: string
  role: string
  sso_required: boolean
  sso_start_url?: string
}
export type Me = {
  id: string
  email: string
  orgs: MeOrg[]
  active_org_id?: string
  is_operator: boolean
}

export async function me(api: APIRequestContext): Promise<Me> {
  const r = await api.get('/api/me')
  expect(r.status(), await r.text()).toBe(200)
  return (await r.json()) as Me
}

/**
 * Asserts a sign-in landed in orgId. The SPA routes an org member under
 * /orgs/<id>/ (the GitHub-identity gate's connect-github page included), and an
 * admin of an org whose setup is unfinished to /setup, which carries no org in
 * its path, so the session's active org is checked too.
 */
export async function expectLandedIn(s: Session, orgId: string) {
  await expect
    .poll(() => {
      const p = new URL(s.page.url()).pathname
      return p.startsWith(`/orgs/${orgId}/`) || p === `/orgs/${orgId}` || p === '/setup'
    })
    .toBe(true)
  expect((await me(s.api)).active_org_id).toBe(orgId)
}

export async function json<T>(
  r: Awaited<ReturnType<APIRequestContext['get']>>,
  status: number,
): Promise<T> {
  expect(r.status(), `${r.url()}: ${await r.text()}`).toBe(status)
  return (await r.json()) as T
}

export async function expectSSORequired(r: Awaited<ReturnType<APIRequestContext['get']>>) {
  const body = await r.text()
  expect(r.status(), `${r.url()}: ${body}`).toBe(403)
  expect(JSON.parse(body).errors?.[0]?.reason).toBe('SSO_REQUIRED')
}

export async function createOrg(api: APIRequestContext, name: string): Promise<string> {
  return (await json<{ id: string }>(await api.post('/api/orgs', { data: { name } }), 201)).id
}

export async function setActiveOrg(api: APIRequestContext, orgId: string) {
  await json(await api.post('/api/me/active-org', { data: { org_id: orgId } }), 200)
}

type ConnectionResponse = {
  connection: {
    id: string
    provider_id: string
    enabled: boolean
    enforced: boolean
    tested: boolean
  } | null
  entity_id: string
  acs_url: string
}

export async function connectSSO(
  api: APIRequestContext,
  metadataURL: string,
): Promise<ConnectionResponse> {
  return json<ConnectionResponse>(
    await api.post('/api/sso/connection', { data: { metadata_url: metadataURL } }),
    201,
  )
}

export async function getSSOConnection(api: APIRequestContext): Promise<ConnectionResponse> {
  return json<ConnectionResponse>(await api.get('/api/sso/connection'), 200)
}

/**
 * Claims domain for the active org and marks it verified in the database.
 * Real DNS cannot serve TXT records for .test names; the TXT check itself is
 * covered by the Postgres tests.
 */
export async function claimVerifiedDomain(api: APIRequestContext, domain: string) {
  const r = await api.post('/api/sso/domains', { data: { domain } })
  expect([200, 201], await r.text()).toContain(r.status())
  psql(`UPDATE sso_domains SET verified_at = now() WHERE domain = '${domain}'`)
}

export async function invite(api: APIRequestContext, email: string): Promise<string> {
  return (
    await json<{ accept_url: string }>(await api.post('/api/invites', { data: { email } }), 201)
  ).accept_url
}

/** Accepts an invite through its link, the way the invitee would. */
export async function acceptInvite(s: Session, acceptURL: string): Promise<string> {
  await s.page.goto(acceptURL)
  await s.page.getByRole('button', { name: 'Accept invitation' }).click()
  await s.page.waitForURL(/\/orgs\/[^/]+/)
  return new URL(s.page.url()).pathname.split('/')[2]
}

export function tokenOf(acceptURL: string): string {
  return new URL(acceptURL).searchParams.get('token') ?? ''
}

export async function mintToken(
  api: APIRequestContext,
  orgId: string,
  name: string,
): Promise<string> {
  return (
    await json<{ token: string }>(
      await api.post('/api/me/tokens', { data: { name, org_id: orgId } }),
      201,
    )
  ).token
}

export async function bearer(token: string): Promise<APIRequestContext> {
  return request.newContext({
    baseURL: BASE,
    extraHTTPHeaders: { Authorization: `Bearer ${token}` },
  })
}

function env(): Record<string, string> {
  const out: Record<string, string> = {}
  for (const line of readFileSync(path.join(STATE, 'env'), 'utf8').split('\n')) {
    const i = line.indexOf('=')
    if (i > 0) out[line.slice(0, i)] = line.slice(i + 1)
  }
  return out
}

export function psql(sql: string): string {
  const container = execFileSync('docker', [
    'ps',
    '-q',
    '--filter',
    'label=com.docker.compose.project=tf-multi-dev',
    '--filter',
    'label=com.docker.compose.service=postgres',
  ])
    .toString()
    .trim()
    .split('\n')[0]
  if (!container) throw new Error('no tf-multi-dev postgres container')
  const dsn = `postgres://supabase_admin:${env().POSTGRES_PASSWORD}@localhost:5432/postgres`
  return execFileSync('docker', [
    'exec',
    container,
    'psql',
    dsn,
    '-v',
    'ON_ERROR_STOP=1',
    '-tAc',
    sql,
  ]).toString()
}

/** Changes a fake GitHub user's email (a test-only route on the fake). */
export function setGitHubEmail(login: string, email: string) {
  execFileSync('curl', [
    '-fsS',
    '--cacert',
    path.join(STATE, 'ca.pem'),
    '--resolve',
    'github.test:8445:127.0.0.1',
    '-X',
    'PUT',
    '-H',
    'Content-Type: application/json',
    '--data',
    JSON.stringify({ email }),
    `https://github.test:8445/users/${encodeURIComponent(login)}`,
  ])
}
