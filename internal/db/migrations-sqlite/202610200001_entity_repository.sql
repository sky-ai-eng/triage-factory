-- +goose Up
-- entities.repository_id: the registry row of the repository a GitHub entity
-- belongs to. The key ("owner/repo#18") names the repository by the name it has
-- now, which a rename changes; this does not. The store sets it when it creates
-- the row, from the key, through the registry's get-or-create, so a pull
-- request in a repository no team tracks has a row to reference.
--
-- Required for a GitHub row. Postgres says so with a CHECK; SQLite cannot add a
-- table CHECK without rebuilding entities, so two triggers refuse an insert or
-- an update that leaves a GitHub row without one. A migration that rebuilds
-- entities drops them with the old table, and has to create them again (or
-- turn them into the CHECK) on the new one.
ALTER TABLE entities ADD COLUMN repository_id TEXT REFERENCES repositories(id);

-- Every existing GitHub row's repository, read off its key the way
-- db.GitHubEntityRepoRef reads it: the part before '#' is owner/repo, split at
-- the first '/', on the host the row's scope is. Every GitHub key a release
-- wrote has that shape: the tracker and the dashboard backfill build
-- "owner/repo#N" from the repository they polled, and the exec funnel records
-- only a key domain.ParsePRTarget accepts.
CREATE TEMP TABLE github_entity_repo AS
SELECT entity_id, org_id, host, created_at,
       substr(path, 1, instr(path, '/') - 1) AS owner,
       substr(path, instr(path, '/') + 1)    AS repo
  FROM (SELECT id AS entity_id, org_id, scope AS host, created_at,
               CASE WHEN instr(source_id, '#') > 0
                    THEN substr(source_id, 1, instr(source_id, '#') - 1)
                    ELSE source_id END AS path
          FROM entities
         WHERE source = 'github')
 WHERE instr(path, '/') > 1 AND instr(path, '/') < length(path);

-- A repository with no registry row gets a bare one, identity columns only, as
-- the store's get-or-create would mint it. The folded identity index makes the
-- first insert for a repository the one that stands, so the newest key's
-- casing is the one a new row stores. A row is not tracking: nothing is added
-- to what TF polls, profiles or clones.
INSERT OR IGNORE INTO repositories (id, owner, repo, source, host, org_id, updated_at)
SELECT lower(hex(randomblob(4))) || '-' || lower(hex(randomblob(2))) || '-4' ||
       substr(lower(hex(randomblob(2))), 2) || '-' ||
       substr('89ab', abs(random()) % 4 + 1, 1) ||
       substr(lower(hex(randomblob(2))), 2) || '-' || lower(hex(randomblob(6))),
       owner, repo, 'github', host, org_id, CURRENT_TIMESTAMP
  FROM github_entity_repo
 ORDER BY created_at DESC, entity_id;

UPDATE entities
   SET repository_id = (
       SELECT r.id
         FROM github_entity_repo g
         JOIN repositories r
           ON r.org_id = g.org_id AND r.source = 'github' AND r.host = g.host
          AND LOWER(r.owner) = LOWER(g.owner) AND LOWER(r.repo) = LOWER(g.repo)
        WHERE g.entity_id = entities.id)
 WHERE source = 'github';

DROP TABLE github_entity_repo;

-- +goose StatementBegin
CREATE TRIGGER entities_repository_required_insert
BEFORE INSERT ON entities
FOR EACH ROW WHEN NEW.source = 'github' AND NEW.repository_id IS NULL
BEGIN
    SELECT RAISE(ABORT, 'entities_repository_required: a github entity references its repository');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER entities_repository_required_update
BEFORE UPDATE ON entities
FOR EACH ROW WHEN NEW.source = 'github' AND NEW.repository_id IS NULL
BEGIN
    SELECT RAISE(ABORT, 'entities_repository_required: a github entity references its repository');
END;
-- +goose StatementEnd

-- Serves the joins that scope a GitHub entity by its repository.
CREATE INDEX idx_entities_repository ON entities(repository_id) WHERE repository_id IS NOT NULL;

-- +goose Down
SELECT 'down not supported';
