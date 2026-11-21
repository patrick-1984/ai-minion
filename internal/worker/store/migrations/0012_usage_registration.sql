-- An existing enrollment gives no implicit permission for expanded telemetry.
ALTER TABLE hub_enrollment ADD COLUMN usage_policy INTEGER NOT NULL DEFAULT 0;
ALTER TABLE hub_enrollment ADD COLUMN usage_required INTEGER NOT NULL DEFAULT 1;
-- Preserve the explicit prior sharing agreement on enrolled installations.
-- A new enrollment and any reassignment need the current disclosure.
UPDATE hub_enrollment SET usage_required=0;
CREATE TRIGGER usage_registration_changed AFTER UPDATE OF hub_pub,worker_id,owner ON hub_enrollment
WHEN OLD.hub_pub<>NEW.hub_pub OR OLD.worker_id<>NEW.worker_id OR OLD.owner<>NEW.owner
BEGIN
    UPDATE hub_enrollment SET usage_policy=0,usage_required=1 WHERE id=NEW.id;
END;
-- Only projected numeric and identity metadata from an explicitly configured
-- native exporter. No raw native status document or message bodies are stored.
CREATE TABLE usage_native (
    session_name TEXT NOT NULL,
    native_id TEXT NOT NULL,
    observed_at INTEGER NOT NULL,
    body TEXT NOT NULL,
    account TEXT,
    PRIMARY KEY(session_name,native_id)
) WITHOUT ROWID;
CREATE INDEX usage_native_age ON usage_native(observed_at);
CREATE TABLE usage_native_events (
    seq INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id TEXT NOT NULL UNIQUE,
    at INTEGER NOT NULL,
    body TEXT NOT NULL
);
CREATE INDEX usage_native_events_age ON usage_native_events(at);
CREATE TRIGGER usage_native_reset AFTER DELETE ON hub_enrollment
BEGIN
    DELETE FROM usage_native;
    DELETE FROM usage_native_events;
END;
CREATE TRIGGER usage_native_new_owner AFTER UPDATE OF hub_pub,worker_id,owner ON hub_enrollment
WHEN OLD.hub_pub<>NEW.hub_pub OR OLD.worker_id<>NEW.worker_id OR OLD.owner<>NEW.owner
BEGIN
    DELETE FROM usage_native;
    DELETE FROM usage_native_events;
END;
