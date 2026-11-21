-- A session-bound nomination is deliberately outside the name-only grant.
-- Its inactive revision survives revocation and replacement.
CREATE TABLE bound_master_session (
    worker_id TEXT NOT NULL REFERENCES workers(id) ON DELETE CASCADE,
    session_name TEXT NOT NULL,
    incarnation_id TEXT NOT NULL,
    owner_id TEXT NOT NULL,
    worker_pub BLOB NOT NULL,
    generation INTEGER NOT NULL CHECK(generation > 0),
    active INTEGER NOT NULL CHECK(active IN (0,1)),
    legacy_targets_json TEXT NOT NULL DEFAULT '',
    changed_at INTEGER NOT NULL,
    PRIMARY KEY(worker_id, session_name)
) WITHOUT ROWID;

CREATE TABLE bound_master_request (
    request_id TEXT PRIMARY KEY,
    worker_id TEXT NOT NULL REFERENCES workers(id) ON DELETE CASCADE,
    actor TEXT NOT NULL,
    decision TEXT NOT NULL
);

-- Also fence the v0.8 API's compatibility writer and direct store writers.
CREATE TRIGGER bound_master_no_legacy_insert BEFORE INSERT ON master_session
WHEN EXISTS (SELECT 1 FROM bound_master_session b WHERE b.worker_id=NEW.worker_id AND b.session_name=NEW.session_name)
BEGIN
    SELECT RAISE(ABORT, 'bound master slot');
END;

CREATE TRIGGER bound_master_no_legacy_update BEFORE UPDATE ON master_session
WHEN EXISTS (SELECT 1 FROM bound_master_session b WHERE b.worker_id=NEW.worker_id AND b.session_name=NEW.session_name)
BEGIN
    SELECT RAISE(ABORT, 'bound master slot');
END;

-- Even moving back to a prior owner or enrollment never revives its old
-- decision. Keep the name's revision fence while the Worker ID survives.
CREATE TRIGGER bound_master_enrollment_changed AFTER UPDATE OF owner_id,worker_pub ON workers
WHEN NEW.owner_id <> OLD.owner_id OR NEW.worker_pub <> OLD.worker_pub
BEGIN
    UPDATE bound_master_session
    SET owner_id=NEW.owner_id, worker_pub=NEW.worker_pub, active=0,
        generation=CASE WHEN generation<9223372036854775807 THEN generation+1 ELSE generation END
    WHERE worker_id=NEW.id;
END;
