-- Steps: the journal of work inside a run. Each model call and each tool call
-- is one step, numbered 1, 2, 3 within its run. See "Steps" in docs/concepts.md.

-- +goose Up
CREATE TABLE steps (
    run_id          uuid NOT NULL REFERENCES runs (id),
    seq             integer NOT NULL CHECK (seq > 0),
    kind            text NOT NULL CHECK (kind IN ('model', 'tool')),
    status          text NOT NULL CHECK (status IN
        ('started', 'awaiting_approval', 'completed', 'failed', 'denied', 'unknown')),

    -- Tool steps only: which tool, and the model's ID for the call, which
    -- links the result back to the request in the conversation.
    tool_name       text,
    tool_use_id     text,

    -- Fixed when the step starts, before any call, so a retry sends the same
    -- key and the tool can ignore the duplicate (Phase F).
    idempotency_key text NOT NULL,

    request         jsonb NOT NULL,
    result          jsonb,
    error           text,

    input_tokens    bigint NOT NULL DEFAULT 0,
    output_tokens   bigint NOT NULL DEFAULT 0,
    cost_micro_usd  bigint NOT NULL DEFAULT 0,

    started_at      timestamptz NOT NULL DEFAULT now(),
    finished_at     timestamptz,

    PRIMARY KEY (run_id, seq),
    CHECK ((kind = 'tool') = (tool_name IS NOT NULL))
);

-- +goose Down
DROP TABLE steps;
