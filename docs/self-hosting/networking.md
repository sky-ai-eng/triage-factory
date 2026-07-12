# Client IP & trusted proxies

TF records the client IP in three places, all fed by one extractor:

1. `sessions.ip_addr` — session forensics.
2. `auth_events.ip_address` — the SOC2 authentication audit log.
3. The pre-auth **per-IP rate limiter** — a brute-force / abuse control. A
   spoofable key is a real security hole: rotate fake values to evade the per-IP
   cap, or stamp a victim's IP to get them throttled.

`X-Forwarded-For` is *appended* by each proxy in the path, never overwritten — so
its leftmost entry is whatever the original caller typed, i.e. attacker-controlled.
TF therefore trusts `X-Forwarded-For` **only** when the request's direct peer is a
proxy you've declared trusted, and then walks the header right-to-left (newest hop
first), skipping trusted hops, to the first untrusted address — the real client.

Configure it with two env vars (multi mode only — local mode is single-user and
ignores them):

- **`TF_TRUSTED_PROXY_CIDR`** — comma-separated CIDRs of your trusted upstream
  proxies (a bare IP is accepted, treated as a `/32` or `/128`). Determines which
  direct peers unlock `X-Forwarded-For`. An IPv4 CIDR also matches a dual-stack
  proxy whose connections arrive as IPv4-mapped IPv6 (`::ffff:10.0.0.1`, common
  with nginx on Linux and AWS ALB); add IPv6 CIDRs only for proxies that connect
  over native IPv6.
- **`TF_CAPTURE_CLIENT_IP`** — boolean, default `true`. Set `false` to capture no
  IP at all (store `NULL`), for data-minimization-conscious deployments.

| Deployment | Config | Result |
| -- | -- | -- |
| **SaaS / behind a stable edge** | `TF_TRUSTED_PROXY_CIDR` = LB/CDN egress range | accurate, unspoofable |
| **Self-host, directly exposed** | leave unset | `RemoteAddr` = real peer, unspoofable |
| **Self-host, behind your own LB** | their proxy CIDR(s) | accurate, unspoofable |
| **Privacy-sensitive** | `TF_CAPTURE_CLIENT_IP=false` (or `TF_TRUSTED_PROXY_CIDR=none`) | `NULL` IP |

**Secure default:** with `TF_TRUSTED_PROXY_CIDR` unset, `X-Forwarded-For` is
ignored entirely and the client IP is the direct peer (`RemoteAddr`). That's never
spoofable — but if TF actually *is* behind a proxy, every request collapses onto
the proxy's IP: the per-IP rate limiter becomes one global bucket (throttling
everyone or no one) and the audit log records the LB, not the client. So **any
proxied deployment must set `TF_TRUSTED_PROXY_CIDR`** for the limiter and audit IPs
to work per-client. Multi mode logs a loud warning at boot when it's unset.

**Edge header hygiene (complement).** Where you control the edge, configure the
outermost proxy to *strip* any inbound `X-Forwarded-For` before appending its own,
so a client can't pre-seed the chain. Examples:

```nginx
# nginx: replace (don't append) — sets XFF to just the real peer
proxy_set_header X-Forwarded-For $remote_addr;
```

```
# HAProxy: option forwardfor already overwrites by default;
# be explicit if you've customized it:
http-request set-header X-Forwarded-For %[src]
```

The right-to-left walk is sound even without the strip (a pre-seeded value sits to
the left of the address your first trusted proxy appends, so it's never reached) —
but stripping at the edge is defense-in-depth and keeps the header honest for
anything else that reads it. This mirrors the outbound `X-Forwarded-For` stripping
TF already does in its git/LLM proxies.
