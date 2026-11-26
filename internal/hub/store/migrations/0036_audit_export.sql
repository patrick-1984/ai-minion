-- Audit rows that SQL writes by itself (the grant triggers of 0034, fired by
-- a demotion, a Worker reassignment or removal, also inside the start-up IdP
-- reconciliation) never pass Store.InsertAudit, so the audit observer never
-- saw them and they never reached the log sinks.
--
-- exported is 1 for a row the observer has seen. Store.InsertAudit writes 1
-- (it reports the row after the commit itself); any other insert keeps the
-- default 0, and the store forwards those rows to the observer after each
-- commit and when the observer is set, then marks them 1 (exactly once).
ALTER TABLE audit_events ADD COLUMN exported INTEGER NOT NULL DEFAULT 0;
-- Rows from before this version are history: they are not exported again.
UPDATE audit_events SET exported = 1;
CREATE INDEX audit_events_unexported ON audit_events (exported) WHERE exported = 0;
