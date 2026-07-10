-- Runs with search_path scoped to the plugin's own schema
-- ("guestbook"), so the unqualified table name lands there.
CREATE TABLE IF NOT EXISTS entries (
    id         BIGSERIAL PRIMARY KEY,
    author     TEXT        NOT NULL,
    message    TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
