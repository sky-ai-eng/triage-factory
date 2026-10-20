import { expect, test, type Page } from '@playwright/test'
import {
  BASE,
  GLOBEX_METADATA,
  MALLORY_METADATA,
  acceptInvite,
  bearer,
  claimVerifiedDomain,
  connectSSO,
  createOrg,
  expectLandedIn,
  expectSSORequired,
  getSSOConnection,
  idpUser,
  invite,
  json,
  me,
  mintToken,
  setGitHubEmail,
  signInByEmail,
  signInGitHub,
  signInThroughProvider,
  tokenOf,
  type Session,
} from './helpers'

// The scenarios run in order against one stack, and later ones read what
// earlier ones built.
test.describe.configure({ mode: 'serial' })

let admin: Session
let globexId: string
let globexProvider: string
let alice: Session
let bobGitHub: Session
let bobSSO: Session
let bobId: string
let bobsOrgId: string
let contractor: Session
let contractorToken: string
let contractorSocket: Page
let mallory: Session
let malloryProvider: string
let requiredAt: number

type Identities = { methods: { provider: string; email?: string }[] }

test('1. an admin connects SSO and tests it', async ({ browser }) => {
  admin = await signInGitHub(browser, 'globex-admin')
  await expect(admin.page).toHaveURL(/\/onboarding$/)
  await admin.page.locator('#org-name').fill('Globex')
  await admin.page.getByRole('button', { name: 'Start your Factory' }).click()
  await admin.page.waitForURL(/\/setup/)
  globexId = (await me(admin.api)).active_org_id!
  expect(globexId).toBeTruthy()

  const created = await connectSSO(admin.api, GLOBEX_METADATA)
  expect(created.entity_id).toBe(`${BASE}/auth/v1/sso/saml/metadata`)
  expect(created.acs_url).toBe(`${BASE}/auth/v1/sso/saml/acs`)
  expect(created.connection?.enabled).toBe(true)
  expect(created.connection?.tested).toBe(false)
  globexProvider = created.connection!.provider_id

  // The settings screen's Test SSO opens this route in a popup.
  const runTest = async () => {
    const popup = await admin.context.newPage()
    await popup.goto('/api/sso/connection/test')
    // The IdP asks for credentials once per browser context.
    const form = popup.locator('input[name="user"]')
    const passed = popup.getByRole('heading', { name: 'SSO test passed' })
    await expect(form.or(passed)).toBeVisible()
    if (await form.isVisible()) {
      const u = idpUser('globex', 'alice')
      await form.fill(u.user)
      await popup.locator('input[name="password"]').fill(u.password)
      await popup.locator('input[type="submit"]').click()
    }
    await expect(popup.getByRole('heading', { name: 'SSO test passed' })).toBeVisible()
    await expect(popup.getByText('alice@globex.test')).toBeVisible()
    return popup
  }

  // Before globex.test is verified, the asserted address would not join an
  // existing account.
  let popup = await runTest()
  await expect(popup.getByText('Matches existing accounts')).toBeVisible()
  await expect(popup.getByText(/^No — globex\.test is not a verified domain/)).toBeVisible()
  await popup.close()
  expect((await getSSOConnection(admin.api)).connection?.tested).toBe(true)

  await claimVerifiedDomain(admin.api, 'globex.test')

  popup = await runTest()
  await expect(popup.getByText('Yes — globex.test is verified for your organization')).toBeVisible()
  await popup.close()
})

test('2. identifier-first sign-in provisions an SSO user into the org', async ({ browser }) => {
  alice = await signInByEmail(browser, idpUser('globex', 'alice'))
  await expectLandedIn(alice, globexId)
  const m = await me(alice.api)
  expect(m.email).toBe('alice@globex.test')
  expect(m.orgs.map((o) => [o.id, o.role])).toEqual([[globexId, 'member']])
})

test('3. an SSO sign-in at a verified domain joins the existing account', async ({ browser }) => {
  const acceptURL = await invite(admin.api, 'bob@globex.test')
  bobGitHub = await signInGitHub(browser, 'bob')
  expect(await acceptInvite(bobGitHub, acceptURL)).toBe(globexId)
  bobId = (await me(bobGitHub.api)).id

  bobSSO = await signInByEmail(browser, idpUser('globex', 'bob'))
  await expectLandedIn(bobSSO, globexId)
  const m = await me(bobSSO.api)
  expect(m.id).toBe(bobId)
  expect(m.orgs.map((o) => o.id)).toContain(globexId)
  const ids = await json<Identities>(await bobSSO.api.get('/api/me/identities'), 200)
  expect(ids.methods.map((x) => x.provider).sort()).toEqual(['github', 'saml'])

  // State that scenarios 8 to 11 need from before Require SSO is turned on.
  const contractorInvite = await invite(admin.api, 'contractor@gmail.test')
  contractor = await signInGitHub(browser, 'contractor')
  expect(await acceptInvite(contractor, contractorInvite)).toBe(globexId)
  // Its own page, so later navigation of the contractor's app page leaves the
  // socket open.
  contractorSocket = await contractor.context.newPage()
  await contractorSocket.goto('/api/health')
  await contractorSocket.evaluate(
    () =>
      new Promise<void>((resolve, reject) => {
        const ws = new WebSocket('ws://localhost:3000/api/ws')
        const w = window as unknown as { __ws: { opened: boolean; code?: number; at?: number } }
        w.__ws = { opened: false }
        ws.onopen = () => {
          w.__ws.opened = true
          resolve()
        }
        ws.onclose = (e) => {
          w.__ws.code = e.code
          w.__ws.at = Date.now()
          if (!w.__ws.opened) reject(new Error(`websocket closed during handshake: ${e.code}`))
        }
      }),
  )
  contractorToken = await mintToken(contractor.api, globexId, 'contractor e2e')
  expect((await (await bearer(contractorToken)).get(`/api/orgs/${globexId}`)).status()).toBe(200)
  bobsOrgId = await createOrg(bobGitHub.api, "Bob's org")
})

test('4. Require SSO turns away a GitHub sign-in on the domain, but not break-glass', async ({
  browser,
}) => {
  const r = await admin.api.patch('/api/sso/connection/enforcement', { data: { enforced: true } })
  expect(r.status(), await r.text()).toBe(200)
  requiredAt = Date.now()
  expect((await r.json()).connection.enforced).toBe(true)

  const bob = await signInGitHub(browser, 'bob')
  await expect(bob.page).toHaveURL(/\/login\?error=sso_required/)
  await expect(bob.page.getByRole('alert')).toContainText(
    'Your organization requires single sign-on',
  )
  await bob.context.close()

  const owner = await signInGitHub(browser, 'globex-admin')
  await expectLandedIn(owner, globexId)
  await owner.context.close()
})

test('5. another org cannot take over an account through its own IdP', async ({ browser }) => {
  mallory = await signInGitHub(browser, 'mallory')
  await createOrg(mallory.api, 'Mallory')
  malloryProvider = (await connectSSO(mallory.api, MALLORY_METADATA)).connection!.provider_id
  const malloryOrgId = (await me(mallory.api)).active_org_id!

  const fake = await signInThroughProvider(browser, malloryProvider, idpUser('mallory', 'bob'))
  const m = await me(fake.api)
  expect(m.id).not.toBe(bobId)
  expect(m.orgs.map((o) => o.id)).toEqual([malloryOrgId])
  await fake.context.close()

  const real = await me(bobSSO.api)
  expect(real.id).toBe(bobId)
  expect(real.orgs.map((o) => o.id).sort()).toEqual([globexId, bobsOrgId].sort())
  const ids = await json<Identities>(await bobSSO.api.get('/api/me/identities'), 200)
  expect(ids.methods.map((x) => x.provider).sort()).toEqual(['github', 'saml'])
})

test('6. operator status follows a verified address, not an asserted one', async ({ browser }) => {
  const fake = await signInThroughProvider(browser, malloryProvider, idpUser('mallory', 'ops'))
  expect((await me(fake.api)).is_operator).toBe(false)
  expect((await fake.api.get('/api/fleet/instances')).status()).toBe(404)
  await fake.context.close()

  const ops = await signInGitHub(browser, 'ops')
  expect((await me(ops.api)).is_operator).toBe(true)
  expect((await ops.api.get('/api/fleet/instances')).status()).toBe(200)
  await ops.context.close()
})

test('7. an invite cannot be accepted with an address another IdP asserts', async ({ browser }) => {
  const acceptURL = await invite(admin.api, 'carol@globex.test')
  const fake = await signInThroughProvider(browser, malloryProvider, idpUser('mallory', 'carol'))
  const r = await fake.api.post('/api/invites/accept', { data: { token: tokenOf(acceptURL) } })
  expect(r.status(), await r.text()).toBe(409)
  expect((await r.json()).errors[0].reason).toBe('INVITE_EMAIL_MISMATCH')
  await fake.context.close()

  const list = await json<{ items: { email: string }[] }>(
    await admin.api.post('/api/invites/list', { data: {} }),
    200,
  )
  expect(list.items.map((i) => i.email)).toContain('carol@globex.test')
})

test('8. an off-domain member is held to the org SSO requirement', async () => {
  await expectSSORequired(await contractor.api.get(`/api/orgs/${globexId}`))
  const globex = (await me(contractor.api)).orgs.find((o) => o.id === globexId)
  expect(globex?.sso_required).toBe(true)
  expect(globex?.sso_start_url).toContain(`provider_id=${globexProvider}`)

  await contractor.page.goto(`/orgs/${globexId}/`)
  await expect(
    contractor.page.getByRole('heading', { name: 'This organization requires single sign-on' }),
  ).toBeVisible()
  await expect(
    contractor.page.getByRole('link', { name: 'Sign in with single sign-on' }),
  ).toBeVisible()
})

test('9. after an email change, GitHub sign-in reaches only the orgs that do not require SSO', async ({
  browser,
}) => {
  setGitHubEmail('bob', 'bob@personal.test')
  const bob = await signInGitHub(browser, 'bob')
  expect(new URL(bob.page.url()).pathname).not.toBe('/login')
  expect((await me(bob.api)).id).toBe(bobId)
  await expectSSORequired(await bob.api.get(`/api/orgs/${globexId}`))
  await json(await bob.api.get(`/api/orgs/${bobsOrgId}`), 200)
  await bob.context.close()
})

test('10. requiring SSO closes an open websocket of a member it bars', async () => {
  const state = await contractorSocket.evaluate(
    () => (window as unknown as { __ws?: { opened: boolean; code?: number; at?: number } }).__ws,
  )
  expect(state?.opened).toBe(true)
  expect(state?.code).toBe(4002)
  expect(state!.at! - requiredAt).toBeLessThanOrEqual(10_000)
})

test('11. tokens carry the sign-in they were minted from', async () => {
  const token = await mintToken(alice.api, globexId, 'alice e2e')
  expect((await (await bearer(token)).get(`/api/orgs/${globexId}`)).status()).toBe(200)
  await expectSSORequired(await (await bearer(contractorToken)).get(`/api/orgs/${globexId}`))
})
