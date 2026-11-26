-- Short session codes "<worker number>-<session number>", for example "5-667"
-- (v0.23.0): a code that is easy to read aloud and to dictate.
--
-- worker_numbers gives every Worker a number, sequential per Hub. A row is
-- never deleted, not even when its Worker is removed, so a number is never
-- given again (the next one is MAX(number) + 1). next_session is the counter
-- of the Worker's session numbers: it only grows, so a session number is
-- never given again within that Worker, also after its mapping was pruned.
CREATE TABLE worker_numbers (
    worker_id    TEXT    PRIMARY KEY,
    number       INTEGER NOT NULL UNIQUE CHECK (number > 0),
    next_session INTEGER NOT NULL DEFAULT 1 CHECK (next_session > 0)
);

-- The Workers enrolled before this version, in the order of their enrollment.
INSERT INTO worker_numbers (worker_id, number)
SELECT id, ROW_NUMBER() OVER (ORDER BY created_at, id) FROM workers;

-- A new Worker gets its number in the very transaction that enrolls it.
CREATE TRIGGER workers_assign_number AFTER INSERT ON workers
BEGIN
    INSERT OR IGNORE INTO worker_numbers (worker_id, number)
    VALUES (NEW.id, (SELECT COALESCE(MAX(number), 0) + 1 FROM worker_numbers));
END;

-- session_codes maps a session number of a Worker to the session it was given
-- to. instance_id is the host-attested identity of the terminal session
-- (sessions_list, v0.22.0) when known: the number then follows a rename. A
-- row without it identifies the session by its name. name is the session's
-- current (last seen) name. Rows not seen for a while are pruned; their
-- numbers are not given again (worker_numbers.next_session).
CREATE TABLE session_codes (
    worker_id   TEXT    NOT NULL,
    number      INTEGER NOT NULL CHECK (number > 0),
    instance_id TEXT,
    name        TEXT    NOT NULL,
    first_seen  INTEGER NOT NULL, -- unix milliseconds
    last_seen   INTEGER NOT NULL, -- unix milliseconds
    PRIMARY KEY (worker_id, number)
);
CREATE UNIQUE INDEX session_codes_instance ON session_codes (worker_id, instance_id) WHERE instance_id IS NOT NULL;
CREATE INDEX session_codes_name ON session_codes (worker_id, name);
CREATE INDEX session_codes_last_seen ON session_codes (last_seen);
