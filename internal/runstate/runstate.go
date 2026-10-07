// Package runstate defines the lifecycle of a run: its states, the events that
// move it between states, and which moves are legal.
//
// It is a pure package: no database, no network, no clock. The store calls
// Transition before writing a state change, so an illegal change is rejected
// before anything is saved. The rules here mirror docs/concepts.md; if the two
// disagree, one of them has a bug.
package runstate

import (
	"errors"
	"fmt"
)

// State is where a run is in its lifecycle.
type State string

// Run states. See the state machine in docs/concepts.md.
const (
	// StateNone is the "state" of a run that does not exist yet.
	StateNone State = ""

	StateQueued         State = "queued"          // waiting for a worker
	StateRunning        State = "running"         // a worker holds its lease
	StateSuspended      State = "suspended"       // waiting for a human approval
	StateOutcomeUnknown State = "outcome_unknown" // a tool may have run; a human must say
	StateSucceeded      State = "succeeded"       // final
	StateFailed         State = "failed"          // final
	StateCancelled      State = "cancelled"       // final
)

// Event is something that happened to a run and may change its state. The
// same names are used as event types in the events table.
//
// Only events that change state are listed here. Events such as "a step
// started" are recorded too, but they are not part of the state machine.
type Event string

// State-changing events. Each constant's value is its name without the prefix.
const (
	EventRunCreated        Event = "RunCreated"
	EventRunClaimed        Event = "RunClaimed"
	EventRunReclaimed      Event = "RunReclaimed"
	EventApprovalRequested Event = "ApprovalRequested"
	EventApprovalDecided   Event = "ApprovalDecided"
	EventApprovalExpired   Event = "ApprovalExpired"
	EventOutcomeUnknown    Event = "OutcomeUnknown"
	EventOutcomeResolved   Event = "OutcomeResolved"
	EventRunSucceeded      Event = "RunSucceeded"
	EventRunFailed         Event = "RunFailed"
	EventRunCancelled      Event = "RunCancelled"
)

// ErrIllegalTransition is returned (wrapped) by Transition. Check for it with
// errors.Is(err, runstate.ErrIllegalTransition).
var ErrIllegalTransition = errors.New("illegal run state transition")

// transitions lists every legal move: transitions[from][event] = to.
// Any pair missing from this table is illegal.
var transitions = map[State]map[Event]State{
	StateNone: {
		EventRunCreated: StateQueued,
	},
	StateQueued: {
		EventRunClaimed:   StateRunning,
		EventRunCancelled: StateCancelled,
	},
	StateRunning: {
		// A new worker takes over after the old worker's lease expired.
		// The state stays the same; the lease epoch goes up.
		EventRunReclaimed:      StateRunning,
		EventApprovalRequested: StateSuspended,
		EventOutcomeUnknown:    StateOutcomeUnknown,
		EventRunSucceeded:      StateSucceeded,
		EventRunFailed:         StateFailed,
		// Only the worker records this, at a safe point between steps.
		EventRunCancelled: StateCancelled,
	},
	StateSuspended: {
		// Approved, denied, or expired: the run goes back to the queue either
		// way. A denial reaches the model as a tool error, not a failed run.
		EventApprovalDecided: StateQueued,
		EventApprovalExpired: StateQueued,
		EventRunCancelled:    StateCancelled,
	},
	StateOutcomeUnknown: {
		EventOutcomeResolved: StateQueued,
		EventRunCancelled:    StateCancelled,
	},
	// Final states accept no events, so they have no entry.
}

// Transition returns the state a run moves to when ev happens in state from.
// It returns an error wrapping ErrIllegalTransition if the move is not allowed.
func Transition(from State, ev Event) (State, error) {
	to, ok := transitions[from][ev] // indexing a missing inner map is safe: ok is false
	if !ok {
		return from, fmt.Errorf("%w: %s in state %q", ErrIllegalTransition, ev, from)
	}
	return to, nil
}

// Final reports whether no further transitions are possible.
func (s State) Final() bool {
	return s == StateSucceeded || s == StateFailed || s == StateCancelled
}

// HoldsLease reports whether a worker owns the run in this state. Only a
// running run occupies a worker; every other state is waiting or finished.
func (s State) HoldsLease() bool {
	return s == StateRunning
}

// States returns every real state (not StateNone), for tests and validation.
func States() []State {
	return []State{
		StateQueued, StateRunning, StateSuspended, StateOutcomeUnknown,
		StateSucceeded, StateFailed, StateCancelled,
	}
}

// Events returns every state-changing event, for tests and validation.
func Events() []Event {
	return []Event{
		EventRunCreated, EventRunClaimed, EventRunReclaimed,
		EventApprovalRequested, EventApprovalDecided, EventApprovalExpired,
		EventOutcomeUnknown, EventOutcomeResolved,
		EventRunSucceeded, EventRunFailed, EventRunCancelled,
	}
}
