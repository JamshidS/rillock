-- First schema: agents, tools, runs, and the event log.
-- Steps arrive in Phase C and approvals in Phase G, as their own migrations.
-- Field meanings are in docs/concepts.md.

-- +goose Up

-- An agent is a name. Everything that can change lives in its versions.
CREATE TABLE agents (
    name       text PRIMARY KEY,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Immutable. `rillock apply` adds a version only when spec_hash differs from
-- the latest version's hash.
CREATE TABLE agent_versions (
    id         uuid PRIMARY KEY DEFAULT uuidv7(),
    agent_name text NOT NULL REFERENCES agents (name),
    version    integer NOT NULL CHECK (version > 0),
    spec       jsonb NOT NULL,
    spec_hash  text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (agent_name, version)
);

CREATE TABLE tools (
    name       text PRIMARY KEY,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Immutable, like agent_versions. effect_class is a column (not only inside
-- spec) because recovery logic depends on it and the database should refuse
-- unknown classes.
CREATE TABLE tool_versions (
    id           uuid PRIMARY KEY DEFAULT uuidv7(),
    tool_name    text NOT NULL REFERENCES tools (name),
    version      integer NOT NULL CHECK (version > 0),
    effect_class text NOT NULL CHECK (effect_class IN
        ('read_only', 'idempotent', 'keyed', 'compensatable', 'non_idempotent')),
    spec         jsonb NOT NULL,
    spec_hash    text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tool_name, version)
);

-- A run is also a queue entry: workers claim rows from this table (ADR 0003).
CREATE TABLE runs (
    id               uuid PRIMARY KEY DEFAULT uuidv7(),
    agent_version_id uuid NOT NULL REFERENCES agent_versions (id),
    input            jsonb NOT NULL,
    state            text NOT NULL CHECK (state IN
        ('queued', 'running', 'suspended', 'outcome_unknown',
         'succeeded', 'failed', 'cancelled')),

    -- Lease: which worker owns the run, until when, and the fencing epoch.
    -- The epoch goes up on every claim and never goes down.
    lease_owner      text,
    lease_epoch      bigint NOT NULL DEFAULT 0,
    lease_expires_at timestamptz,

    cancel_requested boolean NOT NULL DEFAULT false,

    -- Usage totals, checked against the agent's limits. Money is stored as
    -- whole micro-dollars (1 USD = 1,000,000) to avoid floating-point rounding.
    steps_used       integer NOT NULL DEFAULT 0,
    input_tokens     bigint  NOT NULL DEFAULT 0,
    output_tokens    bigint  NOT NULL DEFAULT 0,
    cost_micro_usd   bigint  NOT NULL DEFAULT 0,

    result           jsonb,
    failure_reason   text,

    -- Sequence number of the latest event. The next event gets
    -- last_event_seq + 1, assigned while this row is locked.
    last_event_seq   integer NOT NULL DEFAULT 0,

    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);

-- Workers look for queued runs and running runs whose lease has expired.
CREATE INDEX runs_claimable ON runs (created_at) WHERE state IN ('queued', 'running');

-- Tool versions are pinned when a run is submitted, so changing a tool never
-- changes the rules of a run that already started.
CREATE TABLE run_tool_versions (
    run_id          uuid NOT NULL REFERENCES runs (id),
    tool_version_id uuid NOT NULL REFERENCES tool_versions (id),
    PRIMARY KEY (run_id, tool_version_id)
);

-- The journal. Append-only: see the trigger below.
CREATE TABLE events (
    run_id     uuid NOT NULL REFERENCES runs (id),
    seq        integer NOT NULL CHECK (seq > 0),
    type       text NOT NULL,
    v          smallint NOT NULL DEFAULT 1, -- payload schema version
    payload    jsonb NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (run_id, seq)
);

-- Invariant 2: events are never changed or removed. Enforcing it in the
-- database means a bug in Go code cannot break it either.
-- +goose StatementBegin
CREATE FUNCTION events_append_only() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'events are append-only: % is not allowed', TG_OP
        USING ERRCODE = 'restrict_violation';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER events_no_update_or_delete
    BEFORE UPDATE OR DELETE ON events
    FOR EACH ROW EXECUTE FUNCTION events_append_only();

CREATE TRIGGER events_no_truncate
    BEFORE TRUNCATE ON events
    FOR EACH STATEMENT EXECUTE FUNCTION events_append_only();

-- +goose Down
DROP TABLE events;
DROP FUNCTION events_append_only;
DROP TABLE run_tool_versions;
DROP TABLE runs;
DROP TABLE tool_versions;
DROP TABLE tools;
DROP TABLE agent_versions;
DROP TABLE agents;
