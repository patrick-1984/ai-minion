-- The last instance of every session name of a Worker (v0.23.0, A115): the
-- host-attested identity of the terminal (term.Session.Instance, the
-- instance_id of sessions_list) that the Worker last reported under the name.
-- The event hub compares a reported instance with it, also after a Hub
-- restart, so a name that was killed and created again (also while the Hub was
-- down or the Worker offline) is a new terminal: it is first seen at that
-- moment and what was set up for the old terminal is purged. A Worker that
-- reports no instance (older Workers, psmux) has no rows. The rows of a Worker
-- go with it (workers.EventHub.Forget and the trigger below).
CREATE TABLE session_instance (
    worker_id    TEXT    NOT NULL,
    session_name TEXT    NOT NULL,
    instance_id  TEXT    NOT NULL CHECK (instance_id <> ''),
    seen_at      INTEGER NOT NULL,
    PRIMARY KEY (worker_id, session_name)
);
CREATE INDEX session_instance_seen ON session_instance (worker_id, seen_at);
CREATE TRIGGER session_instance_worker_removed AFTER DELETE ON workers
BEGIN
    DELETE FROM session_instance WHERE worker_id = OLD.id;
END;
