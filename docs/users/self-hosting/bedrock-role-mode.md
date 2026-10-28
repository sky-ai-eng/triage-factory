# Bedrock role mode

With role mode, an org connects Amazon Bedrock by naming an IAM role in its own
AWS account instead of storing a Bedrock key. Triage Factory stores only the
role ARN and an External ID it generates, and for each run or model call it
assumes the role to get a credential that lasts at most an hour and can call
only that org's model. The calls show up in the org's CloudTrail under the
run's id. Bearer-token, access-key and Anthropic-key connections are
unaffected by anything on this page.

## Prerequisite: an AWS identity for the control service

The control service assumes each org's role as itself, so it needs an AWS
identity that can call `sts:AssumeRole`. Executors need none. Either:

- **an instance role** (EC2 instance profile, ECS task role, EKS IRSA, Fly.io
  machine role), which keeps AWS credentials out of `.env`; or
- **`AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` and `AWS_REGION`** on the
  control service. They are a deployment secret, like the rest of `.env`.

## What an org does

In its Claude credentials settings, an org admin picks Bedrock with role auth
and runs the setup. Triage Factory shows the External ID and a trust policy
naming your control service's identity, which the admin adds to their role.
Connecting tests a real AssumeRole and says which side is wrong:

- **no ambient AWS identity:** the control service has neither an instance
  role nor `AWS_*` variables. That's yours to fix.
- **AssumeRole denied:** the org's role doesn't trust your control identity or
  doesn't require the External ID. That's the org's trust policy to fix.

## Settings

All optional, read by the control service.

| Variable | Default | Meaning |
|---|---|---|
| `TF_LLM_CRED_TTL_SEC` | `3600` | Lifetime of each credential, in seconds. Minimum `900`, maximum one hour. |
| `TF_EXECUTOR_EGRESS_CIDRS` | unset | Comma-separated CIDRs of your executors' egress. A credential sent to an executor then works only from those addresses. |
| `TF_EXECUTOR_VPCE_IDS` | unset | Comma-separated `vpce-…` ids. The PrivateLink equivalent of the CIDR setting. |

The network restriction applies only to credentials sent to executors. The
control service's own Bedrock calls (scoring, repo profiling, the connect test)
are not restricted.
