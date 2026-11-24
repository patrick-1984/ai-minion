-- The name the user gave a harness conversation (Claude Code /rename),
-- sanitized and bounded, per pane generation and grant-scoped native session
-- hash. title is the name shown now: the status line's current name once the
-- transcript confirmed it ('' while it is not, or when there is none).
-- verified is the last name the transcript confirmed, so a repeat of it needs
-- no new search. No path, prompt or generated title is stored: checked is a
-- digest of the last status-line name the transcript could not confirm (with
-- the transcript file's identity) and scanned the transcript offset searched
-- for it so far, so the exporter reads only what the transcript gained since.
-- at is the observation time of the latest status line: every observation
-- advances it, and a confirmation is shown only while its observation is
-- still the latest (an older callback that finishes last changes nothing).
-- owner_id is the stable Hub id of the owner from the latest welcome (owner
-- is only its display name, and two accounts can share one): the monitoring
-- grant and the purge below are bound to it, so a reassignment to another
-- account with the same display name still drops every stored name.
ALTER TABLE hub_enrollment ADD COLUMN owner_id TEXT NOT NULL DEFAULT '';
CREATE TABLE harness_titles (
    session TEXT NOT NULL, pane TEXT NOT NULL, created INTEGER NOT NULL,
    harness TEXT NOT NULL, native TEXT NOT NULL,
    title TEXT NOT NULL, verified TEXT NOT NULL DEFAULT '', checked TEXT NOT NULL DEFAULT '', scanned INTEGER NOT NULL DEFAULT 0, at INTEGER NOT NULL,
    PRIMARY KEY(session,pane,created,harness,native)
);
CREATE TRIGGER harness_titles_reset AFTER DELETE ON hub_enrollment BEGIN DELETE FROM harness_titles; END;
CREATE TRIGGER harness_titles_revoke AFTER UPDATE OF hub_pub,worker_id,owner,owner_id,usage_policy ON hub_enrollment
WHEN OLD.hub_pub<>NEW.hub_pub OR OLD.worker_id<>NEW.worker_id OR OLD.owner_id<>NEW.owner_id OR OLD.owner<>NEW.owner OR NEW.usage_policy<>1
BEGIN DELETE FROM harness_titles; END;
