# Bedrock role mode: short-lived LLM credentials

Most Bedrock deployments hand Triage Factory a long-lived credential — a bearer token or an AWS access-key pair — that TF stores (encrypted, per org) and replays on every model call. **Role mode** is the alternative for teams who don't want a standing Bedrock secret living in TF at all: the org configures only a customer **IAM Role ARN** and a TF-generated **External ID**, stores no secret, and TF mints a fresh, short-lived credential per unit of work by assuming that role. Bearer-token Bedrock, access-key Bedrock, and the Anthropic-key path are unchanged passthrough — TF stores and replays those credentials exactly as before, and nothing in this page applies to them.

## Prerequisite — the control service needs an ambient AWS identity

Minting runs on the control ("brain") process, and it assumes the customer role *as itself* — there is no assume-with-a-customer-static-key path. So the control service must be able to call `sts:AssumeRole` under its own AWS identity, via one of:

- **An instance role** — an EC2 instance profile, ECS task role, EKS IRSA, or a Fly.io machine role. The best option: nothing AWS-related lands in `.env`.
- **`AWS_*` environment variables** (`AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`, plus `AWS_REGION`) set on the control service. Treat these as a **deployment secret in the same class as the GoTrue signing material** — they belong in the operator's `.env`, not the keychain, because they are the deployment's own identity, not a per-user integration credential. Executors need no AWS identity of their own; only the control process assumes roles.

## How a mint works

For each unit of work — a delegated run, and each brain-side model call (scoring, repo profiling) — the control process calls `sts:AssumeRole` on the org's configured role ARN, passing the org's External ID and a `RoleSessionName` of the run id, and attaches an **inline session policy** that scopes the resulting credential to `bedrock:InvokeModel` and `bedrock:InvokeModelWithResponseStream` on that org's configured model and nothing else. The credential is short-lived (`TF_LLM_CRED_TTL_SEC`, one hour max) and every call lands in the customer's own CloudTrail attributed to the run (`RoleSessionName = run id`). Because the org stores no secret, there is nothing in TF's database to leak; the worst an exfiltrated session credential buys is minutes of `InvokeModel` on a single model — optionally pinned to your egress (below).

## The customer's trust policy — the role-setup endpoint

The customer's IAM role must trust *your* control identity and require the External ID. `POST /api/bedrock/role-setup` returns the caller ARN your control process actually assumes as, the org's External ID, and a copyable trust-policy JSON snippet the customer pastes into their role — so they don't hand-assemble the `Principal` + `sts:ExternalId` condition themselves.

## The connect probe distinguishes the two failure classes

`POST /api/bedrock/connect` with `auth_method=role` does a *live* `AssumeRole` and reports which side is misconfigured:

- **No ambient identity** — the control service itself has no AWS identity to assume *from*. This is an **operator** problem (missing instance role / `AWS_*`), not the customer's; fix it on the control service.
- **AssumeRole denied** — the control identity is fine, but the customer's role won't let it in. This is a **trust-policy or External-ID** problem on the customer's side; re-check the role's trust relationship against the role-setup snippet.

## Env knobs

All three are read on the control service and all are optional.

- **`TF_LLM_CRED_TTL_SEC`** — lifetime, in seconds, of each minted STS session credential. Default `3600`; mints are capped at one hour and floored at `900` (15 minutes). Lower it to shrink the window an exfiltrated credential stays valid.
- **`TF_EXECUTOR_EGRESS_CIDRS`** — comma-separated CIDRs of your executors' egress. When set, an **executor-bound** mint carries a network condition (`aws:SourceIp`) in its session policy, so a credential lifted off an executor is unusable from anywhere but that egress. Unset → no network condition.
- **`TF_EXECUTOR_VPCE_IDS`** — comma-separated `vpce-…` VPC-endpoint ids, the PrivateLink equivalent of the CIDR knob: an executor-bound mint is pinned to `aws:SourceVpce`. Unset → no network condition.

The network binding applies only to **executor-bound** mints — the credential that travels to a sandbox host. **Brain-bound** mints (the scorer, the repo profiler, and the connect probe, which call Bedrock from the control process itself) carry no network condition, since there is no separate executor egress to pin them to.
