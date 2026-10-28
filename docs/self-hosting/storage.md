# Object storage (SeaweedFS)

Multi mode keeps two kinds of data in an S3-compatible object store, in two
buckets on one endpoint:

| Bucket (variable, default) | Holds | Written by | Read by | Versioning |
|---|---|---|---|---|
| `TF_WORKSPACE_BUCKET`, `tf-workspaces` | Workspace snapshots | Executors | Executors | Off |
| `TF_KNOWLEDGE_BUCKET`, `tf-knowledge` | Team knowledge-base documents | Control pods | Control pods, executors | On |

A blueprint's workspace — its run root, with every repository checkout beneath
it and the scratch space its steps hand off through — must survive the executor
that created it (an open run can outlast the process; an executor can scale down
mid-run). The TF binary snapshots that workspace to the workspace bucket and
rehydrates it on resume, rebuilding each checkout; the host copy is only a warm
cache. A snapshot is a cache too: one per task, under `<org>/<task>/workspace.tar`,
overwritten by a live run every `TF_SNAPSHOT_INTERVAL_SEC` and dropped by TF's
retention reaper. Losing one costs a run some work.

A knowledge-base document is the only copy of what someone wrote. It wants
versioning and backups.

The two are kept apart for two reasons:

- **Versioning is a bucket setting.** Turning it on for one shared bucket would
  keep every snapshot overwrite as a version; leaving it off leaves the
  knowledge base unversioned.
- **Each bucket has its own key pair, so each role holds only the access it
  uses.** Control pods never touch snapshots, and get no workspace keys.
  Executors only read the knowledge base: they copy it into each run at claim.
  Given the read-only pair, a compromised executor cannot rewrite the knowledge
  that every later run on the deployment is staged with.

An executor refuses to boot when `TF_WORKSPACE_BUCKET` and `TF_KNOWLEDGE_BUCKET`
name the same bucket.

Local mode runs none of this: it writes snapshots to `~/.triagefactory/blobs`,
keeps knowledge as plain files under the state root, and reads none of the
object-store variables below. (The eviction settings under
[Executor disk](#executor-disk-evicting-idle-parked-workspaces) apply in local
mode too.)

## Configuration

| Variable | Read by | Default |
|---|---|---|
| `TF_BLOB_ENDPOINT` | Control, executor | `http://seaweedfs:8333` in the bundled compose; required |
| `TF_BLOB_REGION` | Control, executor | `us-east-1` |
| `TF_WORKSPACE_BUCKET` | Executor | `tf-workspaces` |
| `TF_WORKSPACE_ACCESS_KEY`, `TF_WORKSPACE_SECRET_KEY` | Executor | None |
| `TF_KNOWLEDGE_BUCKET` | Control, executor | `tf-knowledge` |
| `TF_KNOWLEDGE_ACCESS_KEY`, `TF_KNOWLEDGE_SECRET_KEY` | Control (the read-write pair), executor (the read-only pair) | None |

Every process reads the same variable names; the deployment decides which pair
each one gets. In the bundled compose, `.env` holds the knowledge writer as
`TF_KNOWLEDGE_ACCESS_KEY` / `TF_KNOWLEDGE_SECRET_KEY` and the reader as
`TF_KNOWLEDGE_READER_ACCESS_KEY` / `TF_KNOWLEDGE_READER_SECRET_KEY`, and the
executor service's `TF_KNOWLEDGE_*` are mapped from the reader pair. The control
service gets no `TF_WORKSPACE_*` at all.

A key pair is both keys or neither. With neither, the binary uses the AWS
default credential chain (`AWS_*` env, the shared credentials file, then an
EC2/ECS instance role), and the role's IAM policy does the scoping. The bundled
compose requires every key, so an instance role means a deployment of your own.

`TF_BLOB_ENDPOINT` is a full URL: its scheme selects TLS, and any base path
(Supabase Storage's `/storage/v1/s3`) is kept.

## The bundled SeaweedFS

Self-host runs **SeaweedFS**: one self-contained S3 container (Apache-2.0, Go),
with no Postgres or JWT coupling. The `seaweedfs` service in `docker-compose.yml`
runs `weed server -s3` (the all-in-one master+volume+filer+S3 process). Its
wrapper writes an `s3.json` with four identities, built from the pairs in
`.env`, each limited to its bucket:

| Identity | Key pair | Bucket | Actions | Held by |
|---|---|---|---|---|
| Workspace | `TF_WORKSPACE_*` | `tf-workspaces` | Read, Write, List | Executor |
| Knowledge writer | `TF_KNOWLEDGE_*` | `tf-knowledge` | Read, Write, List | Control |
| Knowledge reader | `TF_KNOWLEDGE_READER_*` | `tf-knowledge` | Read, List | Executor |
| Admin | `SEAWEEDFS_ADMIN_*` | Both | Admin | `seaweedfs-postinit` only |

Configuring identities is also what takes SeaweedFS **out of its open-by-default
"allow-all" mode**: the bundled store never serves an unauthenticated request.
SeaweedFS finds an identity by its access key, so the wrapper refuses to start
unless the four access keys differ. The S3 port (`8333`) is **not** published to
the host; TF and the sidecar reach it only over the compose network.

The one-shot `seaweedfs-postinit` sidecar runs on every `up`. It creates each
bucket that is missing (`head-bucket || create-bucket` via aws-cli), and turns
versioning on for `tf-knowledge`. It never versions `tf-workspaces`.

SeaweedFS has no separate permission for versioning: its `Write` action also
covers `PutBucketVersioning` and deleting a specific version. So the knowledge
writer — the control pods' pair — can suspend versioning on `tf-knowledge` and
permanently delete old versions; the reader pair can do neither. The sidecar
sets versioning on every `up`, not only when it creates the bucket, so a
suspension lasts until the next `up`, but a deleted version is gone. In the
bundled stack, then, versioning keeps knowledge recoverable from overwrites and
deletes made through TF, not from someone holding the writer pair. On S3 the
writer's policy leaves both actions out (see below).

Smoke-test a round trip through the aws-cli sidecar, which holds the admin pair
(the stack must already be up):

```sh
docker compose run --rm --no-deps --entrypoint sh seaweedfs-postinit -c '
  EP=http://seaweedfs:8333; B=$TF_KNOWLEDGE_BUCKET
  echo hello | aws --endpoint-url "$EP" s3 cp - "s3://$B/smoke.txt" &&
  aws --endpoint-url "$EP" s3 cp "s3://$B/smoke.txt" - &&            # -> hello
  aws --endpoint-url "$EP" s3 rm "s3://$B/smoke.txt"'
```

To look at stored objects, list them through the same sidecar:
`docker compose run --rm --no-deps --entrypoint sh seaweedfs-postinit -c 'aws
--endpoint-url http://seaweedfs:8333 s3 ls "s3://$TF_WORKSPACE_BUCKET/"'`.

## Executor disk: evicting idle parked workspaces

The workspace bucket bounds the **durable** copy of a workspace (snapshots are
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
holds a live claim, and the whole key has been idle past the TTL. The TTL is
the one condition disk pressure drops: when free space falls below
`TF_DISPATCH_DISK_FLOOR_MB` (default 10240; 0 disables), the executor stops
claiming new work and, once a minute, evicts trees that meet every other
condition until free space is back above the floor. This runs even with
`TF_WORKSPACE_EVICT_AFTER_SEC=0`. Anything else
— a persist still in flight, a persist that failed, a blob that is missing
despite the record — leaves the tree alone, because it is then the only copy of
the agent's uncommitted work. An evicted workspace is not lost: the next resume
rehydrates it from the snapshot, exactly as it does after an executor restart.

## Hosted Supabase Storage, S3, or R2 (SaaS / BYO)

The same `aws-sdk-go-v2` client (path-style addressing, configurable
`BaseEndpoint`) drives **any** S3-protocol backend, so pointing it elsewhere
needs no compose change. Pre-create both buckets, turn versioning on for the
knowledge bucket where the provider supports it, set the three key pairs to keys
scoped as below, and override the endpoint (and bucket names / region as needed)
in `.env`:

```sh
TF_BLOB_ENDPOINT=https://<ref>.supabase.co/storage/v1/s3   # base path IS preserved
TF_BLOB_REGION=us-east-1
TF_WORKSPACE_BUCKET=tf-workspaces
TF_KNOWLEDGE_BUCKET=tf-knowledge
```

The bundled `seaweedfs` service still starts in this configuration but goes
unused; drop it via a compose override (and remove `seaweedfs-postinit` from the
`control` and `executor` services' `depends_on` in that override) if you don't
want it running. The `seaweedfs-postinit` sidecar only provisions the bundled
SeaweedFS.

### What each role needs

| Role | Workspace bucket | Knowledge bucket |
|---|---|---|
| Control | Nothing | Read, write, delete, list |
| Executor | Read, write, delete, list | Read, list |

On S3, as IAM policies (replace the bucket names with yours). Each role's
policy is attached to the key pair, or instance role, that role is given. An
executor holds two pairs, so it takes the workspace policy and the knowledge
reader's; with an instance role, that role carries both.

Workspace pair (executors):

```json
{
  "Version": "2012-10-17",
  "Statement": [
    { "Effect": "Allow", "Action": "s3:ListBucket", "Resource": "arn:aws:s3:::tf-workspaces" },
    {
      "Effect": "Allow",
      "Action": ["s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:AbortMultipartUpload"],
      "Resource": "arn:aws:s3:::tf-workspaces/*"
    }
  ]
}
```

Knowledge writer (control pods):

```json
{
  "Version": "2012-10-17",
  "Statement": [
    { "Effect": "Allow", "Action": "s3:ListBucket", "Resource": "arn:aws:s3:::tf-knowledge" },
    {
      "Effect": "Allow",
      "Action": ["s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:AbortMultipartUpload"],
      "Resource": "arn:aws:s3:::tf-knowledge/*"
    }
  ]
}
```

Knowledge reader (executors):

```json
{
  "Version": "2012-10-17",
  "Statement": [
    { "Effect": "Allow", "Action": "s3:ListBucket", "Resource": "arn:aws:s3:::tf-knowledge" },
    { "Effect": "Allow", "Action": "s3:GetObject", "Resource": "arn:aws:s3:::tf-knowledge/*" }
  ]
}
```

Every role needs `s3:ListBucket` on its buckets, even where it would not
otherwise list: without it, S3 answers a request for a missing key with 403
instead of 404, and TF reads that as a failure rather than an absent object.
Uploads are multipart, which `s3:PutObject` covers apart from aborting one.

None of these grants `s3:PutBucketVersioning` or `s3:DeleteObjectVersion`, so
no TF role can suspend versioning or remove an old version. On a versioned
bucket TF's deletes only add a delete marker, and the document stays
recoverable. Versioning keeps every version of every document; add a lifecycle
rule expiring noncurrent versions after a period you choose if that growth
matters. Versioning protects against overwrites and deletes made through TF,
not against losing the bucket, so back it up as you would a database.

### A provider that cannot scope a key to one bucket

Give every role the same key pair. TF works unchanged, and the two buckets still
keep the snapshots and the knowledge apart, but the credential split is gone: an
executor can write the knowledge base, and a control pod can write snapshots.

- **Supabase Storage.** Its S3 access keys give full access to every bucket in
  the project and bypass storage RLS, so there is no per-bucket or read-only key
  to hand an executor. Supabase Storage also does not support S3 versioning, so
  the split separates the two kinds of data but gives neither the credential
  separation nor versioning. Back the knowledge bucket up yourself, for example
  with a scheduled `aws s3 sync` of it to storage TF does not write.
- **Cloudflare R2.** An R2 API token can be limited to specific buckets, with
  object read-only or read-write permission, so the three pairs above can be
  scoped. R2 does not implement `PutBucketVersioning`, so back the knowledge
  bucket up yourself, as for Supabase.

## Team knowledge bases (multi mode)

The knowledge bucket is where each **team's knowledge base** lives — the
documents people upload on the Knowledge page — under keys of the form:

```
<orgID>/teams/<teamID>/kb/private/<path…>
<orgID>/teams/<teamID>/kb/shared/<path…>
```

**Visibility is the prefix, not a setting.** A document under `private/` is
readable only through its own team's gate; one under `shared/` is readable by
any member of the org. Publishing is a move between the two roots, which is why
there is no visibility column in the database and nothing to keep in step with
the object store. The layout under each root is an ordinary folder tree; every
path segment is validated (no traversal, no separators inside a segment, no dot
files) at the one place keys are built, `internal/kbstore`.

Control pods serve the read and write routes through the store, with the
writer pair. An executor stages a run's knowledge from it at claim, with the
reader pair — the task team's whole KB plus every other team's `shared/` root,
copied into the run tree under `_tfac/knowledge/`. There is no pod-local durable
copy; a staged tree is rebuilt from the store on every launch and is
deliberately excluded from a parked run's workspace snapshot, so nothing an agent
writes there reaches the knowledge base.

Knowledge is binary-safe: the same multipart uploader that carries workspace
snapshots carries an uploaded document, so a diagram or a screenshot files
alongside a runbook.

Local mode keeps knowledge as plain files under the state root
(`~/.triagefactory/teams/<teamID>/kb/{private,shared}/…`) and constructs no blob
store for it — the on-disk copy is the durable copy, and an operator can edit
their own team's notes with an ordinary text editor.
