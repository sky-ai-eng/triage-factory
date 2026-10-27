# Connecting an identity provider (SAML SSO)

For org admins. Your deployment's operator has to enable SAML first
([deployment setup](../self-hosting/sso.md)). The steps below use Microsoft
Entra; other SAML providers take the same values under their own names, which
**Settings → Single sign-on** shows once you pick the provider.

## 1. Create the enterprise application in Entra

In the [Entra admin center](https://entra.microsoft.com): **Enterprise
applications → New application → Create your own application →** "Integrate
any other application you don't find in the gallery". Name it (for example
"Triage Factory"), then open **Single sign-on → SAML**.

**Basic SAML Configuration.** Copy the values from **Settings → Single
sign-on** in Triage Factory:

| Entra field | Value |
|---|---|
| Identifier (Entity ID) | `{TF_PUBLIC_URL}/auth/v1/sso/saml/metadata` |
| Reply URL (Assertion Consumer Service URL) | `{TF_PUBLIC_URL}/auth/v1/sso/saml/acs` |
| Sign on URL | Leave blank for now; you set it in step 4. |

Leave Relay State and Logout Url blank.

**Attributes & Claims.** Sign-in creates accounts from the email in the
assertion, so it must carry a real address. Under **Attributes & Claims →
Edit**, check that:

- the email claim
  (`http://schemas.xmlsoap.org/ws/2005/05/identity/claims/emailaddress`)
  exists, and its source attribute is **`user.mail`**, not
  `user.userprincipalname`;
- your users have **Contact info → Email** filled in. Cloud-only and guest
  accounts often don't, and their sign-ins then carry no email.

The NameID can stay at Entra's default.

## 2. Assign users

Entra only signs in users assigned to the app: **Users and groups → Add
user/group**. Start with a test user.

## 3. Connect in Triage Factory

From **SAML Certificates**, copy **App Federation Metadata Url**. You don't
download the certificate; Triage Factory reads it from this URL. In **Settings
→ Single sign-on**, paste it into the metadata URL field and press
**Connect**.

New members who sign in through the connection join your org as `member`.
Promote them from the People roster.

## 4. Set Entra's Sign on URL

After connecting, **Settings → Single sign-on** shows a **Sign-on URL**. Paste
it into Entra's **Sign on URL** field. The My Apps tile then starts sign-in at
Triage Factory, which is the safer flow than an unsolicited assertion.

## 5. Verify your domains

Under **Settings → Single sign-on**, **Add domain**, create the DNS TXT record
it shows, and press **Verify**. A verified domain does two things:

- people on it can reach your sign-in by typing their email on the login page;
- an SSO sign-in on it joins the Triage Factory account that already holds that
  address, such as a colleague who signs in with GitHub. On an unverified
  domain, each SSO sign-in gets a separate account that belongs to your org
  only.

A domain verified by a different org doesn't count. If your directory reassigns
an address on a verified domain to another person, that person's first sign-in
joins the previous holder's account.

## 6. Test

Press **Test SSO**. It runs a real sign-in in a new window and reports pass or
fail with the email it received, without creating a session, membership or
account. **Matches existing accounts** says whether that address's domain is
verified for your org; the test passes either way.

The connection is live as soon as you connect it. Turn it off with **Enable
SSO** while you work on it if you need to; a disabled connection can still be
tested.

## Requiring SSO

**Require SSO** means your org is used only through your identity provider:

- a GitHub sign-in with an email on one of your verified domains is refused and
  sent to SSO;
- every request in your org must come from a session or API token signed in
  through your connection, whatever the member's email. This applies to
  existing sessions and tokens, on their next request.

**Break-glass** principals are exempt from both, so you can get in if the
identity provider is down. The first time you require SSO, the org owner is
added if the list is empty.

Members who aren't signed in through your connection see your org's SSO sign-in
instead of its pages. They can't switch into the org, mint a token for it, or
accept an invitation to it. API tokens minted from a GitHub session stop working;
members who use tokens for automation should mint new ones after signing in
with SSO.

Turning it on is refused until all of these hold:

| Refused when | Message |
|---|---|
| The connection is disabled | "enable the connection before requiring SSO" |
| No test has passed | "run a successful Test before requiring SSO" |
| No domain is verified | "verify a domain before requiring SSO" |
| You're neither signed in through the connection nor break-glass | "Sign in through single sign-on, or be designated break-glass, before requiring it." |

While it's on, disabling the connection, removing your last verified domain,
or removing your last break-glass principal is refused. If your SSO
entitlement lapses, the requirement stops applying.

Limits: disabling someone in your identity provider doesn't end their current
session (sessions last 30 days); remove them from the People roster. Delegated
runs, pollers and webhooks are unaffected by the requirement.

Every change on this page is recorded under **Policy** in the Usage page's
"Access & credential changes" band (requires the `governance` entitlement).

## Troubleshooting

| Symptom | Fix |
|---|---|
| The test fails with "the assertion carried no email claim" | Map the user's email to the email claim (step 1), and check the user has an email in Entra. |
| A user signs in but lands on "Start your Factory" instead of your org | The connection is disabled. Turn on **Enable SSO**; their next sign-in joins them to the org. Do this before sending people to SSO, or they may create a separate org from that screen. |
| Connecting fails with a metadata error | Check you pasted the **App Federation Metadata Url** of this app. If it's right, the deployment can't reach your identity provider; ask your operator. |
| You changed the app in Entra, but connecting again changed nothing | Connecting again returns the existing connection without re-reading the metadata. Ask your operator to replace the registration. |
