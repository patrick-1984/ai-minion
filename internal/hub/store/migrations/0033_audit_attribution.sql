-- Audit attribution: the source session of a brokered call, the grant that
-- allowed it and a closed k=v detail. All three are empty for older rows.
ALTER TABLE audit_events ADD COLUMN source TEXT NOT NULL DEFAULT '';
ALTER TABLE audit_events ADD COLUMN grant_id TEXT NOT NULL DEFAULT '';
ALTER TABLE audit_events ADD COLUMN detail TEXT NOT NULL DEFAULT '';
CREATE INDEX audit_events_grant_ts ON audit_events (grant_id, ts DESC, id DESC) WHERE grant_id <> '';
