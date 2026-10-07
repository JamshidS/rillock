package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/jamshids/rillock/internal/runstate"
)

// ErrNotFound is returned (wrapped) when a requested row does not exist.
var ErrNotFound = errors.New("not found")

// inTx runs fn in a database transaction. If fn returns an error, or panics,
// everything it did is rolled back; otherwise it is committed. This is how
// invariant 1 holds: a state change and its event commit together or not at all.
func (s *Store) inTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.pool, fn)
}

// appendEvent adds the next event to a run's journal. It must run inside the
// same transaction as the change it describes.
//
// The UPDATE both increments the run's event counter and locks the run's row
// until the transaction ends, so two transactions can never give out the same
// sequence number.
func appendEvent(ctx context.Context, tx pgx.Tx, runID, eventType string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode %s payload: %w", eventType, err)
	}

	var seq int
	err = tx.QueryRow(ctx, `
		UPDATE runs SET last_event_seq = last_event_seq + 1, updated_at = now()
		WHERE id = $1
		RETURNING last_event_seq`, runID).Scan(&seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("run %s: %w", runID, ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("next event number: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO events (run_id, seq, type, v, payload)
		VALUES ($1, $2, $3, 1, $4)`, runID, seq, eventType, data); err != nil {
		return fmt.Errorf("insert %s event: %w", eventType, err)
	}
	return nil
}

// transition moves a run to the state that ev leads to and records ev, all
// inside tx. runstate.Transition decides whether the move is legal, so the
// rules live in one place. An illegal move returns an error wrapping
// runstate.ErrIllegalTransition and changes nothing.
func transition(ctx context.Context, tx pgx.Tx, runID string, ev runstate.Event, payload any) error {
	// FOR UPDATE locks the row: no other transaction can change this run's
	// state between our read and our write.
	var from runstate.State
	err := tx.QueryRow(ctx, `SELECT state FROM runs WHERE id = $1 FOR UPDATE`, runID).Scan(&from)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("run %s: %w", runID, ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("read run state: %w", err)
	}

	to, err := runstate.Transition(from, ev)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE runs SET state = $2 WHERE id = $1`, runID, to); err != nil {
		return fmt.Errorf("update run state: %w", err)
	}
	return appendEvent(ctx, tx, runID, string(ev), payload)
}
