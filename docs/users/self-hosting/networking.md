# Client IP and trusted proxies

Triage Factory records each request's client IP on sessions and in the
authentication audit log, and keys the sign-in rate limiter by it. Behind a
load balancer or CDN, tell it which proxies to trust, or every request is
attributed to the proxy: the audit log records the proxy, and the rate limiter
puts every client in one bucket. Multi mode logs a warning at boot when this
is unset. Local mode ignores both settings.

| Variable | Meaning |
|---|---|
| `TF_TRUSTED_PROXY_CIDR` | Comma-separated CIDRs (or bare IPs) of your proxies. `X-Forwarded-For` is read only on requests arriving from one of them, and the client is the newest address in it that isn't a trusted proxy. An IPv4 CIDR also matches a proxy connecting over IPv4-mapped IPv6 (common with nginx and AWS ALB). `none` captures no IP. |
| `TF_CAPTURE_CLIENT_IP` | Default `true`. `false` captures no IP at all. |

| Deployment | Setting |
|---|---|
| Exposed directly | Leave unset. The client IP is the connection's peer. |
| Behind your own load balancer or CDN | `TF_TRUSTED_PROXY_CIDR` = its address range |
| Recording no client IPs | `TF_CAPTURE_CLIENT_IP=false` |

If you control the outermost proxy, also have it replace any `X-Forwarded-For`
a client sends rather than append to it:

```nginx
proxy_set_header X-Forwarded-For $remote_addr;
```

```
# HAProxy (option forwardfor already does this unless customized)
http-request set-header X-Forwarded-For %[src]
```
