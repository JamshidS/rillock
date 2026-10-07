package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jamshids/rillock/internal/runstate"
	"github.com/jamshids/rillock/internal/spec"
)

// Run is a run as stored, with its agent's name and version.
type Run struct {
	ID              string
	Agent           string
	AgentVersion    int
	State           runstate.State
	CancelRequested bool
	Input           json.RawMessage
	Result          json.RawMessage // nil until the run succeeds
	FailureReason   string
	StepsUsed       int
	InputTokens     int64
	OutputTokens    int64
	CostMicroUSD    int64
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Event is one entry in a run's journal.
type Event struct {
	Seq       int
	Type      string
	V         int
	Payload   json.RawMessage
	CreatedAt time.Time
}

// EventRunCancelRequested is recorded when a running run is asked to stop. It
// does not change the state: the worker does that at its next safe point.
const EventRunCancelRequested = "RunCancelRequested"

// SubmitRun queues a run of the latest version of the named agent.
//
// In one transaction it creates the run, pins the latest version of each tool
// the agent uses (so later tool changes never affect this run), and records
// RunCreated as event 1.
//
// An empty input is stored as JSON null.
func (s *Store) SubmitRun(ctx context.Context, agent string, input json.RawMessage) (Run, error) {
	if len(input) == 0 {
		input = json.RawMessage("null")
	}
	var run Run
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var versionID string
		var version int
		var a spec.Agent
		err := tx.QueryRow(ctx, `
			SELECT id::text, version, spec FROM agent_versions
			WHERE agent_name = $1 ORDER BY version DESC LIMIT 1`, agent).Scan(&versionID, &version, &a)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("agent %q: %w", agent, ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("look up agent %q: %w", agent, err)
		}

		// The state comes from the state machine, not a hard-coded string.
		state, err := runstate.Transition(runstate.StateNone, runstate.EventRunCreated)
		if err != nil {
			return err
		}
		var runID string
		if err := tx.QueryRow(ctx, `
			INSERT INTO runs (agent_version_id, input, state) VALUES ($1, $2, $3)
			RETURNING id::text`, versionID, input, state).Scan(&runID); err != nil {
			return fmt.Errorf("insert run: %w", err)
		}

		if err := pinTools(ctx, tx, runID, a.Tools); err != nil {
			return err
		}
		payload := map[string]any{"agent": agent, "agentVersion": version}
		if err := appendEvent(ctx, tx, runID, string(runstate.EventRunCreated), payload); err != nil {
			return err
		}

		run, err = getRun(ctx, tx, runID)
		return err
	})
	return run, err
}

// pinTools records the latest version of each tool for the run.
func pinTools(ctx context.Context, tx pgx.Tx, runID string, tools []string) error {
	if len(tools) == 0 {
		return nil
	}
	// DISTINCT ON keeps the first row per tool; ordering by version DESC makes
	// that the latest version.
	tag, err := tx.Exec(ctx, `
		INSERT INTO run_tool_versions (run_id, tool_version_id)
		SELECT $1, id FROM (
			SELECT DISTINCT ON (tool_name) id FROM tool_versions
			WHERE tool_name = ANY($2)
			ORDER BY tool_name, version DESC
		) latest`, runID, tools)
	if err != nil {
		return fmt.Errorf("pin tool versions: %w", err)
	}
	if int(tag.RowsAffected()) != len(tools) {
		// Apply checks that tools exist, so this means the data is inconsistent.
		return fmt.Errorf("pin tool versions: found %d of %d tools", tag.RowsAffected(), len(tools))
	}
	return nil
}

// GetRun returns one run.
func (s *Store) GetRun(ctx context.Context, id string) (Run, error) {
	return getRun(ctx, s.pool, id)
}

// ListRunsFilter narrows ListRuns. The zero value lists the newest runs in any state.
type ListRunsFilter struct {
	State    runstate.State // empty means any state
	IDPrefix string         // only runs whose ID starts with this; empty means any
	Limit    int            // 0 means DefaultListLimit
}

// Limits for list operations.
const (
	DefaultListLimit = 50
	MaxListLimit     = 500
)

// ListRuns returns runs, newest first.
func (s *Store) ListRuns(ctx context.Context, f ListRunsFilter) ([]Run, error) {
	// The prefix is compared with left(...) = $2 rather than LIKE $2 || '%',
	// so characters such as '%' or '_' in it have no special meaning.
	rows, err := s.pool.Query(ctx, runSelect+`
		WHERE ($1 = '' OR r.state = $1)
		  AND ($2 = '' OR left(r.id::text, length($2)) = $2)
		ORDER BY r.id DESC
		LIMIT $3`, f.State, f.IDPrefix, clampLimit(f.Limit))
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	// A CollectableRow can Scan, so it satisfies the pgx.Row that scanRun takes.
	runs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Run, error) { return scanRun(row) })
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	return runs, nil
}

// CancelRun stops a run.
//
// A run that is waiting (queued, suspended, outcome_unknown) is cancelled
// immediately. A running run only gets its cancel flag set: the worker finishes
// and records the step in progress, then cancels the run itself. A run that has
// already finished cannot be cancelled; that returns an error wrapping
// runstate.ErrIllegalTransition.
func (s *Store) CancelRun(ctx context.Context, id string, by string) (Run, error) {
	var run Run
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var state runstate.State
		var alreadyRequested bool
		err := tx.QueryRow(ctx, `SELECT state, cancel_requested FROM runs WHERE id = $1 FOR UPDATE`, id).
			Scan(&state, &alreadyRequested)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("run %s: %w", id, ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("read run: %w", err)
		}

		payload := map[string]any{"by": by}
		switch {
		case state == runstate.StateRunning && alreadyRequested:
			// Nothing to do; asking twice is not an error.
		case state == runstate.StateRunning:
			if _, err := tx.Exec(ctx, `UPDATE runs SET cancel_requested = true WHERE id = $1`, id); err != nil {
				return fmt.Errorf("request cancel: %w", err)
			}
			if err := appendEvent(ctx, tx, id, EventRunCancelRequested, payload); err != nil {
				return err
			}
		default:
			if err := transition(ctx, tx, id, runstate.EventRunCancelled, payload); err != nil {
				return err
			}
		}

		run, err = getRun(ctx, tx, id)
		return err
	})
	return run, err
}

// ListEvents returns a run's events with a sequence number greater than
// afterSeq, oldest first. Pass afterSeq 0 to start from the beginning.
func (s *Store) ListEvents(ctx context.Context, runID string, afterSeq, limit int) ([]Event, error) {
	// Check the run exists, so an unknown run is "not found" rather than "no events".
	if _, err := s.GetRun(ctx, runID); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT seq, type, v, payload, created_at FROM events
		WHERE run_id = $1 AND seq > $2
		ORDER BY seq
		LIMIT $3`, runID, afterSeq, clampLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	events, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Event, error) {
		var e Event
		err := row.Scan(&e.Seq, &e.Type, &e.V, &e.Payload, &e.CreatedAt)
		return e, err
	})
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	return events, nil
}

// querier is what getRun needs: both a pool and a transaction provide it.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

const runSelect = `
	SELECT r.id::text, av.agent_name, av.version, r.state, r.cancel_requested,
	       r.input, r.result, coalesce(r.failure_reason, ''), r.steps_used,
	       r.input_tokens, r.output_tokens, r.cost_micro_usd, r.created_at, r.updated_at
	FROM runs r JOIN agent_versions av ON av.id = r.agent_version_id`

func getRun(ctx context.Context, q querier, id string) (Run, error) {
	run, err := scanRun(q.QueryRow(ctx, runSelect+` WHERE r.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, fmt.Errorf("run %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return Run{}, fmt.Errorf("get run %s: %w", id, err)
	}
	return run, nil
}

func scanRun(row pgx.Row) (Run, error) {
	var r Run
	err := row.Scan(&r.ID, &r.Agent, &r.AgentVersion, &r.State, &r.CancelRequested,
		&r.Input, &r.Result, &r.FailureReason, &r.StepsUsed,
		&r.InputTokens, &r.OutputTokens, &r.CostMicroUSD, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}

func clampLimit(limit int) int {
	switch {
	case limit <= 0:
		return DefaultListLimit
	case limit > MaxListLimit:
		return MaxListLimit
	default:
		return limit
	}
}
