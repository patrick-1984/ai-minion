-- Story 6.6: trust and Hub-side restriction of Workers (AD-12, AD-16, AD-17).

-- Trust (AD-17). One row says that a receiving user reads the output of a
-- Worker, or of one of its sessions, as trusted. session_name is the empty
-- string for the whole Worker. The row only decides whether a model reads
-- output as trusted; it never restricts anybody. worker_id has no foreign
-- key: the workers table belongs to another domain, and every transaction
-- that reassigns or removes a Worker deletes its trust rows itself.
CREATE TABLE trust (
    receiver_user_id TEXT    NOT NULL,
    worker_id        TEXT    NOT NULL,
    session_name     TEXT    NOT NULL DEFAULT '',
    created_at       INTEGER NOT NULL,           -- unix milliseconds
    PRIMARY KEY (receiver_user_id, worker_id, session_name),
    CHECK (receiver_user_id <> '' AND worker_id <> '')
);

CREATE INDEX trust_worker ON trust (worker_id);

-- The Hub-side policy of a Worker, delivered in welcome (AD-12): with
-- preview_only set the Worker refuses every write. The Worker's own local
-- policy is enforced on the Worker whatever the Hub sends.
ALTER TABLE workers ADD COLUMN preview_only INTEGER NOT NULL DEFAULT 0 CHECK (preview_only IN (0, 1));
