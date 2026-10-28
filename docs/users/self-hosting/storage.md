# Object storage

Triage Factory keeps two kinds of data in an S3-compatible object store, in two
buckets on one endpoint:

| Bucket (variable, default) | Holds | Written by | Read by | Versioning |
|---|---|---|---|---|
| `TF_WORKSPACE_BUCKET`, `tf-workspaces` | Workspace snapshots | Executors | Executors | Off |
| `TF_KNOWLEDGE_BUCKET`, `tf-knowledge` | Team knowledge base documents | Control | Control, executors | On |

A run's workspace has to outlive the executor running it, since a run can be
resumed after its executor restarts or scales away. Triage Factory snapshots the
workspace to the workspace bucket and restores it on resume. A snapshot is a
cache: a live run overwrites it every `TF_SNAPSHOT_INTERVAL_SEC` and Triage
Factory deletes it when its retention expires. Losing one costs a run some work.

A knowledge base document is the only copy of what someone wrote, so it wants
versioning and backups.

The two are kept apart for two reasons:

- **Versioning is a bucket setting.** Turning it on for one shared bucket would
  keep every snapshot overwrite as a version.
- **Each bucket has its own key pair, so each service holds only the access it
  uses.** Control never touches snapshots and gets no workspace keys. Executors
  only read the knowledge base (they copy it into each run when they claim it),
  and with a read-only pair a compromised executor cannot rewrite the knowledge
  every later run starts with.

An executor refuses to start when the two bucket names are the same.

Local mode uses none of this. It keeps snapshots under `~/.triagefactory/blobs`
and knowledge as plain files, and reads none of the object store variables
below.

## Configuration

| Variable | Read by | Default |
|---|---|---|
| `TF_BLOB_ENDPOINT` | Control, executor | `http://seaweedfs:8333` in the bundled compose; required |
| `TF_BLOB_REGION` | Control, executor | `us-east-1` |
| `TF_WORKSPACE_BUCKET` | Executor | `tf-workspaces` |
| `TF_WORKSPACE_ACCESS_KEY`, `TF_WORKSPACE_SECRET_KEY` | Executor | None |
| `TF_KNOWLEDGE_BUCKET` | Control, executor | `tf-knowledge` |
| `TF_KNOWLEDGE_ACCESS_KEY`, `TF_KNOWLEDGE_SECRET_KEY` | Control (read-write pair), executor (read-only pair) | None |

Every service reads the same variable names; the deployment decides which pair
each gets. In the bundled compose, `.env` holds the knowledge writer as
`TF_KNOWLEDGE_ACCESS_KEY` / `TF_KNOWLEDGE_SECRET_KEY` and the reader as
`TF_KNOWLEDGE_READER_ACCESS_KEY` / `TF_KNOWLEDGE_READER_SECRET_KEY`, and the
executor's `TF_KNOWLEDGE_*` are mapped from the reader pair.

A key pair is both keys or neither. With neither, Triage Factory uses the AWS
default credential chain (`AWS_*` variables, the shared credentials file, then
an instance role), and the role's IAM policy does the scoping. The bundled
compose requires every key, so an instance role means a deployment of your own.

`TF_BLOB_ENDPOINT` is a full URL; its scheme selects TLS and any base path is
kept.

## The bundled store (SeaweedFS)

The compose stack runs SeaweedFS as a single S3 container. It is configured with
four identities, built from the pairs in `.env`, each limited to its bucket:

| Identity | Key pair | Bucket | Access | Held by |
|---|---|---|---|---|
| Workspace | `TF_WORKSPACE_*` | `tf-workspaces` | Read, write, list | Executor |
| Knowledge writer | `TF_KNOWLEDGE_*` | `tf-knowledge` | Read, write, list | Control |
| Knowledge reader | `TF_KNOWLEDGE_READER_*` | `tf-knowledge` | Read, list | Executor |
| Admin | `SEAWEEDFS_ADMIN_*` | Both | Admin | `seaweedfs-postinit` only |

With identities configured the store never serves anonymous requests. SeaweedFS
finds an identity by its access key, so it refuses to start unless the four
access keys differ. Its S3 port (8333) is not published to the host.

A `seaweedfs-postinit` container runs on every `docker compose up`. It creates
any bucket that is missing and turns versioning on for `tf-knowledge`.

SeaweedFS has no separate permission for versioning: its write access also
allows suspending versioning and deleting a specific version. So the knowledge
writer pair, which control holds, can do both; the reader pair can do neither.
The postinit container turns versioning back on at every `up`, but a deleted
version is gone. In the bundled stack, versioning protects knowledge from
overwrites and deletes made through Triage Factory, not from someone who holds
the writer pair.

To check the store works (with the stack up):

```sh
docker compose run --rm --no-deps --entrypoint sh seaweedfs-postinit -c '
  EP=http://seaweedfs:8333; B=$TF_KNOWLEDGE_BUCKET
  echo hello | aws --endpoint-url "$EP" s3 cp - "s3://$B/smoke.txt" &&
  aws --endpoint-url "$EP" s3 cp "s3://$B/smoke.txt" - &&            # prints hello
  aws --endpoint-url "$EP" s3 rm "s3://$B/smoke.txt"'
```

To list what is stored:

```sh
docker compose run --rm --no-deps --entrypoint sh seaweedfs-postinit -c \
  'aws --endpoint-url http://seaweedfs:8333 s3 ls "s3://$TF_WORKSPACE_BUCKET/"'
```

The knowledge bucket is the only durable copy of knowledge base documents, so
include it in your backups.

## Another S3-compatible store (S3, R2, Supabase Storage)

Create both buckets yourself (the postinit container only provisions the bundled
SeaweedFS), turn versioning on for the knowledge bucket if the provider supports
it, set the three key pairs to keys scoped as below, and override the endpoint,
and the bucket names and region if needed, in `.env`:

```sh
TF_BLOB_ENDPOINT=https://<ref>.supabase.co/storage/v1/s3
TF_BLOB_REGION=us-east-1
TF_WORKSPACE_BUCKET=tf-workspaces
TF_KNOWLEDGE_BUCKET=tf-knowledge
```

The `seaweedfs` service still starts but goes unused; to drop it, remove it in a
compose override, along with `seaweedfs-postinit` from the `depends_on` of
`control` and `executor`.

Whichever store you use, give both buckets a lifecycle rule that aborts
incomplete multipart uploads (see [Scaling](scaling.md)).

### What each service needs

| Service | Workspace bucket | Knowledge bucket |
|---|---|---|
| Control | Nothing | Read, write, delete, list |
| Executor | Read, write, delete, list | Read, list |

On S3 these are IAM policies, attached to the key pair (or instance role) the
service gets. An executor holds two pairs, so it takes the workspace policy and
the knowledge reader's; with an instance role, that role carries both. Replace
the bucket names with yours.

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

Knowledge writer (control):

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

Every service needs `s3:ListBucket` on its buckets, even where it would not
otherwise list: without it, S3 answers a request for a missing key with 403
instead of 404, and Triage Factory reads that as a failure rather than an absent
object.

None of these grants `s3:PutBucketVersioning` or `s3:DeleteObjectVersion`, so no
service can suspend versioning or remove an old version. On a versioned bucket a
delete only adds a delete marker and the document stays recoverable. Versioning
keeps every version of every document; add a lifecycle rule that expires
noncurrent versions after a period you choose if that growth matters.
Versioning does not protect against losing the bucket, so back it up as you
would a database.

### A provider that cannot scope a key to one bucket

Give every service the same key pair. Everything works, and the two buckets
still keep snapshots and knowledge apart, but the credential split is gone: an
executor can write the knowledge base and control can write snapshots.

- **Supabase Storage.** Its S3 access keys reach every bucket in the project, so
  there is no per-bucket or read-only key to give an executor. It also does not
  support S3 versioning. Back the knowledge bucket up yourself, for example with
  a scheduled `aws s3 sync` to storage Triage Factory does not write.
- **Cloudflare R2.** An API token can be limited to specific buckets with
  read-only or read-write permission, so the three pairs can be scoped. R2 does
  not implement bucket versioning, so back the knowledge bucket up yourself, as
  for Supabase.

## Executor disk: parked workspaces

Stored snapshots are deleted 14 days after a run last parked or finished. The
copy on an executor's own disk is managed separately: a parked run's worktree
stays there so a resume on the same executor is instant, and each executor
deletes worktrees that have been idle too long:

| Variable | Default | Effect |
|---|---|---|
| `TF_WORKSPACE_EVICT_AFTER_SEC` | `21600` (6h) | Hourly, delete parked worktrees idle longer than this. `0` turns it off. |
| `TF_DISPATCH_DISK_FLOOR_MB` | `10240` | While free disk is below this many MiB, stop taking new runs and, once a minute, delete parked worktrees without waiting for the idle time. Runs even when `TF_WORKSPACE_EVICT_AFTER_SEC=0`. `0` turns it off. |

A worktree is deleted only when its snapshot is confirmed in the object store
and no live run is using it. Otherwise, it may be the only copy of the agent's
uncommitted work, so it stays. A resume after deletion restores the workspace
from the snapshot.
