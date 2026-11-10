-- Share selection of the Worker (R75): which tmux sessions the Hub may see.
-- share_config holds the mode (no row until the operator has chosen one);
-- share_selected holds the chosen session names, in the order given.
CREATE TABLE share_config (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    mode       TEXT    NOT NULL CHECK (mode IN ('all', 'none', 'select', 'later')),
    updated_at INTEGER NOT NULL
);

CREATE TABLE share_selected (
    session_name TEXT    PRIMARY KEY CHECK (length(session_name) > 0),
    position     INTEGER NOT NULL
);
