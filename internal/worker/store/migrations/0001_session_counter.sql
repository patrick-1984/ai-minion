-- Persisted monotonic counter for Worker-created tmux sessions (minion-N,
-- AD-13). One row; the number is only ever incremented, so a name is never
-- reused, even after its session is gone.
CREATE TABLE session_counter (
    id   INTEGER PRIMARY KEY CHECK (id = 1),
    last INTEGER NOT NULL CHECK (last >= 0)
);

INSERT INTO session_counter (id, last) VALUES (1, 0);
