# Durable workspace storage (SeaweedFS)

A blueprint's workspace — the git worktree plus the scratch space its steps hand
off through — must survive the executor that created it (an open run can outlast
the process; an executor can scale down mid-run). The TF binary snapshots that
workspace to an **S3-compatible object store** and rehydrates it on resume; the
host worktree is only a warm cache.

Self-host runs **SeaweedFS** for this: one self-contained S3 container (Apache-2.0,
Go), no Postgres or JWT coupling. The workspace snapshots are opaque
server-internal tarballs — they need a dumb bucket, not a storage API's RLS /
resumable-upload / CDN layer. The `seaweedfs` service in `docker-compose.yml` runs
`weed server -s3` (the all-in-one master+volume+filer+S3 process), and a one-shot
`seaweedfs-postinit` sidecar creates the bucket on every `up` (`head-bucket ||
create-bucket` via aws-cli — idempotent).

The TF binary talks to it through the `TF_BLOB_*` env (read only in multi mode;
local mode writes blobs to `~/.triagefactory/blobs` and runs none of this). The
compose stack templates a single-identity `s3.json` from the same
`TF_BLOB_ACCESS_KEY` / `TF_BLOB_SECRET_KEY` pair you set in `.env`, so server,
bucket sidecar, and client all share one credential — there's no second input to
drift. Supplying that identity is also what takes SeaweedFS **out of its
open-by-default "allow-all" mode**: the bundled store always requires the creds,
never serving anonymously on the compose network. The S3 port (`8333`) is
deliberately **not** published to the host — TF and the sidecar reach it only over
the compose network.

Smoke-test the round-trip through the aws-cli sidecar (exercises the same bucket
and creds TF's S3 client uses; the stack must already be up):

```sh
docker compose run --rm --no-deps --entrypoint sh seaweedfs-postinit -c '
  EP=http://seaweedfs:8333; B=${TF_BLOB_BUCKET:-tf-workspaces}
  echo hello | aws --endpoint-url "$EP" s3 cp - "s3://$B/smoke.txt" &&
  aws --endpoint-url "$EP" s3 cp "s3://$B/smoke.txt" - &&            # -> hello
  aws --endpoint-url "$EP" s3 rm "s3://$B/smoke.txt"'
```

To eyeball stored snapshot objects there's no published browser console; list them
through the same sidecar: `docker compose run --rm --no-deps --entrypoint sh
seaweedfs-postinit -c 'aws --endpoint-url http://seaweedfs:8333 s3 ls
"s3://${TF_BLOB_BUCKET:-tf-workspaces}/"'`.

## Executor disk: evicting idle parked workspaces

The object store bounds the **durable** copy of a workspace (snapshots are
dropped 14 days after the last park or terminal). The **warm** copy — the actual
worktree on an executor's disk — is bounded separately, because it costs one
machine's disk rather than the fleet's bucket.

A parked tree is kept as the warm-resume cache: a resume that lands back on the
same executor reuses it instead of rehydrating. Left alone, an executor that
never restarts accumulates one full checkout per parked blueprint run and slowly
loses the disk headroom to take new work while its CPU and memory sit idle. So
every executor sweeps hourly and reclaims trees that have been idle past a TTL:

```sh
TF_WORKSPACE_EVICT_AFTER_SEC=21600   # default 6h in multi mode; 0 disables
```

A tree is only ever evicted when the durable snapshot is recorded written *and*
the blob store confirms the object is there, no conversation sharing the tree
holds a live claim, and the whole key has been idle past the TTL. Anything else
— a persist still in flight, a persist that failed, a blob that is missing
despite the record — leaves the tree alone, because it is then the only copy of
the agent's uncommitted work. An evicted workspace is not lost: the next resume
rehydrates it from the snapshot, exactly as it does after an executor restart.

## Hosted Supabase Storage, S3, or R2 (SaaS / BYO)

The same `aws-sdk-go-v2` client (path-style addressing, configurable
`BaseEndpoint`) drives **any** S3-protocol backend — there's no compose change to
point it elsewhere. Set `TF_BLOB_ACCESS_KEY` / `TF_BLOB_SECRET_KEY` to that
backend's keys and override the endpoint (and bucket / region as needed) in
`.env`:

```sh
TF_BLOB_ENDPOINT=https://<ref>.supabase.co/storage/v1/s3   # base path IS preserved
TF_BLOB_BUCKET=tf-workspaces
TF_BLOB_REGION=us-east-1
```

`TF_BLOB_ENDPOINT` is a full URL — its scheme selects TLS and any base path
(Supabase Storage's `/storage/v1/s3`) is kept intact. The bundled `seaweedfs`
service still starts in this configuration but goes unused; drop it via a compose
override (and remove `seaweedfs-postinit` from `triagefactory`'s `depends_on` in
that override) if you don't want it running. Pre-create the bucket on the hosted
side (the `seaweedfs-postinit` sidecar only ensures the bucket on the bundled
SeaweedFS).
