# Fleet console and operators

The fleet console is a deployment-wide view of every Triage Factory process:
role, build version and heartbeat, memory headroom and run-slot usage, run
timings, and LLM spend over 24 hours or 7 days. It is multi-mode only.

## Getting the Fleet tab

The **Fleet** tab appears only when all three hold:

1. the deployment runs in multi mode;
2. the license includes `fleet`;
3. you are a deployment **operator**.

Being an org owner or admin doesn't make you an operator. Operators administer
the whole deployment, so they're granted from the shell, not in the app.

## Granting operator access

Run the `operator` command in the control container (it refuses to run on an
executor):

```sh
docker compose exec control triagefactory operator add you@example.com
docker compose exec control triagefactory operator list
docker compose exec control triagefactory operator remove you@example.com
```

Reload the app after granting; no restart is needed. The console is at
`/<your-org>/fleet`.

- **Use a verified email of one of your sign-in methods.** A GitHub address
  counts when GitHub reports it verified. An SSO address counts only when its
  domain is verified for the org whose connection you signed in through.
- **A grant belongs to an account.** If an account already holds the address
  verified, `add` binds the grant to it and prints its user id. Otherwise the
  grant stays `(pending)` until the first sign-in that verifies the address.
  A bound grant stays with its account even if the address later changes. To
  move a grant to a different account, `remove` and `add` it again.
- Deleting the account deletes the grant.
- Grants and revocations are recorded in the deployment's authentication audit
  log.

## Troubleshooting

| Symptom | Fix |
|---|---|
| Licensed for `fleet`, but no Fleet tab | You're not an operator yet. Run `operator add` and reload. |
| Granted, but still no tab | Run `operator list`. If the grant is bound to another user id, someone else held the address when it was bound: `remove` and `add` it again once your account holds it. If it's `(pending)`, the address doesn't match a verified address of yours; for an SSO address, its domain must be verified for your org. Also check the license includes `fleet`. |
| `operator` fails with a database error | Run it through `docker compose exec control`, where the database settings are. |
