# Workspace storage

A run's workspace has to outlive the executor running it, since a run can be
resumed after its executor restarts or scales away. Triage Factory snapshots
workspaces to an S3-compatible object store and restores them on resume. The
same bucket holds each team's knowledge base documents.

## The bundled store (SeaweedFS)

The compose stack runs SeaweedFS as a single S3 container. A `seaweedfs-postinit`
container creates the bucket on every `docker compose up`.

`TF_BLOB_ACCESS_KEY` and `TF_BLOB_SECRET_KEY` from `.env` are both the store's
only identity and the credentials Triage Factory uses, so the store never
serves anonymous requests. Its S3 port (8333) is not published to the host.

To check the store works (with the stack up):

```sh
docker compose run --rm --no-deps --entrypoint sh seaweedfs-postinit -c '
  EP=http://seaweedfs:8333; B=${TF_BLOB_BUCKET:-tf-workspaces}
  echo hello | aws --endpoint-url "$EP" s3 cp - "s3://$B/smoke.txt" &&
  aws --endpoint-url "$EP" s3 cp "s3://$B/smoke.txt" - &&            # prints hello
  aws --endpoint-url "$EP" s3 rm "s3://$B/smoke.txt"'
```

To list what is stored:

```sh
docker compose run --rm --no-deps --entrypoint sh seaweedfs-postinit -c \
  'aws --endpoint-url http://seaweedfs:8333 s3 ls "s3://${TF_BLOB_BUCKET:-tf-workspaces}/"'
```

The bucket is the only durable copy of knowledge base documents, so include it
in your backups.

## Another S3-compatible store (S3, R2, Supabase Storage)

Set `TF_BLOB_ACCESS_KEY` and `TF_BLOB_SECRET_KEY` to that store's keys and
override the endpoint, and the bucket and region if needed, in `.env`:

```sh
TF_BLOB_ENDPOINT=https://<ref>.supabase.co/storage/v1/s3
TF_BLOB_BUCKET=tf-workspaces
TF_BLOB_REGION=us-east-1
```

`TF_BLOB_ENDPOINT` is a full URL; its scheme selects TLS and any base path is
kept. Create the bucket yourself: the postinit container only creates it on the
bundled SeaweedFS. The `seaweedfs` service still starts but goes unused; to
drop it, remove it in a compose override, along with `seaweedfs-postinit` from
`control`'s `depends_on`.

Whichever store you use, give the bucket a lifecycle rule that aborts
incomplete multipart uploads (see [Scaling](scaling.md)).

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
