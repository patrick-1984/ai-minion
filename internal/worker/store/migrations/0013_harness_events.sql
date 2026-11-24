-- Projected hook inputs only. No command, path, prompt or native identifier.
CREATE TABLE harness_events (
    seq INTEGER PRIMARY KEY,
    session TEXT NOT NULL, pane TEXT NOT NULL, created INTEGER NOT NULL,
    pid INTEGER NOT NULL, birth TEXT NOT NULL,
    harness TEXT NOT NULL, native TEXT NOT NULL, kind TEXT NOT NULL,
    entity TEXT NOT NULL, registry TEXT NOT NULL, at INTEGER NOT NULL
);
CREATE INDEX harness_events_binding ON harness_events(session,pane,created,harness,at DESC,seq DESC);
CREATE INDEX harness_events_generation ON harness_events(session,pane,created,harness,pid,birth,at DESC,seq DESC) WHERE kind='session_start';
CREATE INDEX harness_events_age ON harness_events(at);
CREATE TRIGGER harness_reset AFTER DELETE ON hub_enrollment BEGIN DELETE FROM harness_events; END;
CREATE TRIGGER harness_revoke AFTER UPDATE OF hub_pub,worker_id,owner,usage_policy ON hub_enrollment
WHEN OLD.hub_pub<>NEW.hub_pub OR OLD.worker_id<>NEW.worker_id OR OLD.owner<>NEW.owner OR NEW.usage_policy<>1
BEGIN DELETE FROM harness_events; END;
