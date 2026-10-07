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

// StepKind is what a step does.
type StepKind string

// Step kinds.
const (
	StepModel StepKind = "model"
	StepTool  StepKind = "tool"
)

// StepStatus is where a step is. See "Steps" in docs/concepts.md.
type StepStatus string

// Step statuses used so far. awaiting_approval and unknown arrive with
// approvals (Phase G) and exactly-once recovery (Phase F).
const (
	StepStarted   StepStatus = "started"   // intent recorded; the call may or may not have happened
	StepCompleted StepStatus = "completed" // result recorded
	StepFailed    StepStatus = "failed"    // the call returned an error, recorded
	StepDenied    StepStatus = "denied"    // refused before calling; the call never happened
)

// Event types for steps.
const (
	EventStepStarted   = "StepStarted"
	EventStepCompleted = "StepCompleted"
	EventStepFailed    = "StepFailed"
	EventStepDenied    = "StepDenied"
)

// Step is one recorded model or tool call.
type Step struct {
	Seq            int
	Kind           StepKind
	Status         StepStatus
	ToolName       string
	ToolUseID      string
	IdempotencyKey string
	Request        json.RawMessage
	Result         json.RawMessage
	Error          string
	InputTokens    int64
	OutputTokens   int64
	CostMicroUSD   int64
	StartedAt      time.Time
}

// StepStart describes a step about to begin.
type StepStart struct {
	Kind      StepKind
	ToolName  string          // tool steps only
	ToolUseID string          // tool steps only
	Request   json.RawMessage // what will be sent
}

// StepFinish describes how a step ended.
type StepFinish struct {
	Status       StepStatus // completed, failed, or denied
	Result       json.RawMessage
	Error        string
	Summary      string // a short, human-readable line for the event log
	InputTokens  int64
	OutputTokens int64
	CostMicroUSD int64
}

// ClaimedRun is everything a worker needs to execute a run.
type ClaimedRun struct {
	Run   Run
	Agent spec.Agent
	Tools []spec.Tool // the versions pinned when the run was submitted
}

// ClaimNextRun moves the oldest queued run to running and returns it, or
// returns nil when no run is waiting.
//
// SKIP LOCKED makes concurrent workers skip a row another worker is claiming
// instead of waiting for it. Leases and fencing arrive in Phase D.
func (s *Store) ClaimNextRun(ctx context.Context, workerID string) (*ClaimedRun, error) {
	var claimed *ClaimedRun
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var runID string
		err := tx.QueryRow(ctx, `
			SELECT id::text FROM runs
			WHERE state = 'queued'
			ORDER BY id
			LIMIT 1
			FOR UPDATE SKIP LOCKED`).Scan(&runID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // nothing to do
		}
		if err != nil {
			return fmt.Errorf("find queued run: %w", err)
		}

		if err := transition(ctx, tx, runID, runstate.EventRunClaimed, map[string]any{"worker": workerID}); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE runs SET lease_owner = $2 WHERE id = $1`, runID, workerID); err != nil {
			return fmt.Errorf("record worker: %w", err)
		}

		claimed, err = loadClaimedRun(ctx, tx, runID)
		return err
	})
	return claimed, err
}

func loadClaimedRun(ctx context.Context, tx pgx.Tx, runID string) (*ClaimedRun, error) {
	run, err := getRun(ctx, tx, runID)
	if err != nil {
		return nil, err
	}
	c := &ClaimedRun{Run: run}
	if err := tx.QueryRow(ctx, `
		SELECT av.spec FROM runs r JOIN agent_versions av ON av.id = r.agent_version_id
		WHERE r.id = $1`, runID).Scan(&c.Agent); err != nil {
		return nil, fmt.Errorf("load agent spec: %w", err)
	}

	rows, err := tx.Query(ctx, `
		SELECT tv.spec FROM run_tool_versions rtv JOIN tool_versions tv ON tv.id = rtv.tool_version_id
		WHERE rtv.run_id = $1
		ORDER BY tv.tool_name`, runID)
	if err != nil {
		return nil, fmt.Errorf("load tools: %w", err)
	}
	c.Tools, err = pgx.CollectRows(rows, pgx.RowTo[spec.Tool])
	if err != nil {
		return nil, fmt.Errorf("load tools: %w", err)
	}
	return c, nil
}

// StartStep records that a step is about to begin and returns its number.
// The step is written before the call happens, so that after a crash the
// journal shows a call that may have happened (Phase D and F rely on this).
func (s *Store) StartStep(ctx context.Context, runID string, st StepStart) (int, error) {
	var seq int
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		// steps_used doubles as the step counter; the UPDATE also locks the run.
		if err := tx.QueryRow(ctx, `
			UPDATE runs SET steps_used = steps_used + 1 WHERE id = $1 AND state = 'running'
			RETURNING steps_used`, runID).Scan(&seq); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("run %s is not running: %w", runID, ErrNotFound)
			}
			return fmt.Errorf("next step number: %w", err)
		}

		var toolName, toolUseID *string // NULL for model steps
		if st.Kind == StepTool {
			toolName, toolUseID = &st.ToolName, &st.ToolUseID
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO steps (run_id, seq, kind, status, tool_name, tool_use_id, idempotency_key, request)
			VALUES ($1, $2, $3, 'started', $4, $5, $6, $7)`,
			runID, seq, st.Kind, toolName, toolUseID, IdempotencyKey(runID, seq), st.Request); err != nil {
			return fmt.Errorf("insert step: %w", err)
		}

		payload := map[string]any{"seq": seq, "kind": st.Kind}
		if st.Kind == StepTool {
			payload["tool"] = st.ToolName
			payload["input"] = st.Request
		}
		return appendEvent(ctx, tx, runID, EventStepStarted, payload)
	})
	return seq, err
}

// IdempotencyKey is the key a tool receives for a step. It is fixed when the
// step starts, so every retry of that step sends the same key.
func IdempotencyKey(runID string, seq int) string {
	return fmt.Sprintf("%s:%d", runID, seq)
}

// FinishStep records how a step ended, adds its usage to the run's totals,
// and records the matching event, all in one transaction.
func (s *Store) FinishStep(ctx context.Context, runID string, seq int, f StepFinish) error {
	eventType, ok := map[StepStatus]string{
		StepCompleted: EventStepCompleted,
		StepFailed:    EventStepFailed,
		StepDenied:    EventStepDenied,
	}[f.Status]
	if !ok {
		return fmt.Errorf("finish step: status %q is not a final step status", f.Status)
	}

	return s.inTx(ctx, func(tx pgx.Tx) error {
		var kind StepKind
		var toolName *string
		var durationMs float64
		err := tx.QueryRow(ctx, `
			UPDATE steps SET status = $3, result = $4, error = nullif($5, ''),
			       input_tokens = $6, output_tokens = $7, cost_micro_usd = $8,
			       finished_at = clock_timestamp()
			WHERE run_id = $1 AND seq = $2 AND status = 'started'
			RETURNING kind, tool_name, extract(epoch FROM finished_at - started_at) * 1000`,
			runID, seq, f.Status, nullJSON(f.Result), f.Error, f.InputTokens, f.OutputTokens, f.CostMicroUSD,
		).Scan(&kind, &toolName, &durationMs)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("step %d of run %s is not in progress: %w", seq, runID, ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("finish step: %w", err)
		}

		if _, err := tx.Exec(ctx, `
			UPDATE runs SET input_tokens = input_tokens + $2, output_tokens = output_tokens + $3,
			       cost_micro_usd = cost_micro_usd + $4
			WHERE id = $1`, runID, f.InputTokens, f.OutputTokens, f.CostMicroUSD); err != nil {
			return fmt.Errorf("add usage: %w", err)
		}

		payload := map[string]any{
			"seq": seq, "kind": kind, "durationMs": int64(durationMs), "summary": f.Summary,
			"inputTokens": f.InputTokens, "outputTokens": f.OutputTokens, "costMicroUSD": f.CostMicroUSD,
		}
		if toolName != nil {
			payload["tool"] = *toolName
		}
		if f.Error != "" {
			payload["error"] = f.Error
		}
		return appendEvent(ctx, tx, runID, eventType, payload)
	})
}

// CompleteRun marks a running run succeeded with its result.
func (s *Store) CompleteRun(ctx context.Context, runID string, result json.RawMessage) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE runs SET result = $2 WHERE id = $1`, runID, result); err != nil {
			return fmt.Errorf("store result: %w", err)
		}
		return transition(ctx, tx, runID, runstate.EventRunSucceeded, map[string]any{})
	})
}

// FailRun marks a running run failed and records why.
func (s *Store) FailRun(ctx context.Context, runID, reason string) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE runs SET failure_reason = $2 WHERE id = $1`, runID, reason); err != nil {
			return fmt.Errorf("store failure reason: %w", err)
		}
		return transition(ctx, tx, runID, runstate.EventRunFailed, map[string]any{"reason": reason})
	})
}

// ListSteps returns a run's steps in order.
func (s *Store) ListSteps(ctx context.Context, runID string) ([]Step, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT seq, kind, status, coalesce(tool_name, ''), coalesce(tool_use_id, ''), idempotency_key,
		       request, result, coalesce(error, ''), input_tokens, output_tokens, cost_micro_usd, started_at
		FROM steps WHERE run_id = $1 ORDER BY seq`, runID)
	if err != nil {
		return nil, fmt.Errorf("list steps: %w", err)
	}
	steps, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Step, error) {
		var st Step
		err := row.Scan(&st.Seq, &st.Kind, &st.Status, &st.ToolName, &st.ToolUseID, &st.IdempotencyKey,
			&st.Request, &st.Result, &st.Error, &st.InputTokens, &st.OutputTokens, &st.CostMicroUSD, &st.StartedAt)
		return st, err
	})
	if err != nil {
		return nil, fmt.Errorf("list steps: %w", err)
	}
	return steps, nil
}

// nullJSON stores an empty result as SQL NULL rather than invalid JSON.
func nullJSON(v json.RawMessage) any {
	if len(v) == 0 {
		return nil
	}
	return v
}
