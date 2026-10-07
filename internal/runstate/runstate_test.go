package runstate

import (
	"errors"
	"fmt"
	"testing"
)

// legal is the state machine from docs/concepts.md, written out a second time
// on purpose. If someone changes the table in runstate.go without updating
// this list (and the design doc), the test fails.
var legal = []struct {
	from State
	ev   Event
	to   State
}{
	{StateNone, EventRunCreated, StateQueued},

	{StateQueued, EventRunClaimed, StateRunning},
	{StateQueued, EventRunCancelled, StateCancelled},

	{StateRunning, EventRunReclaimed, StateRunning},
	{StateRunning, EventApprovalRequested, StateSuspended},
	{StateRunning, EventOutcomeUnknown, StateOutcomeUnknown},
	{StateRunning, EventRunSucceeded, StateSucceeded},
	{StateRunning, EventRunFailed, StateFailed},
	{StateRunning, EventRunCancelled, StateCancelled},

	{StateSuspended, EventApprovalDecided, StateQueued},
	{StateSuspended, EventApprovalExpired, StateQueued},
	{StateSuspended, EventRunCancelled, StateCancelled},

	{StateOutcomeUnknown, EventOutcomeResolved, StateQueued},
	{StateOutcomeUnknown, EventRunCancelled, StateCancelled},
}

// TestEveryPair checks every (state, event) combination: the ones in legal
// must succeed with the right target, and every other one must fail.
func TestEveryPair(t *testing.T) {
	want := map[State]map[Event]State{}
	for _, l := range legal {
		if want[l.from] == nil {
			want[l.from] = map[Event]State{}
		}
		want[l.from][l.ev] = l.to
	}

	froms := append([]State{StateNone}, States()...)
	for _, from := range froms {
		for _, ev := range Events() {
			t.Run(fmt.Sprintf("%s/%s", name(from), ev), func(t *testing.T) {
				got, err := Transition(from, ev)
				wantTo, isLegal := want[from][ev]

				switch {
				case isLegal && err != nil:
					t.Fatalf("expected legal move to %q, got error: %v", wantTo, err)
				case isLegal && got != wantTo:
					t.Fatalf("moved to %q, want %q", got, wantTo)
				case !isLegal && err == nil:
					t.Fatalf("expected an illegal transition, but moved to %q", got)
				case !isLegal && !errors.Is(err, ErrIllegalTransition):
					t.Fatalf("error %v does not wrap ErrIllegalTransition", err)
				case !isLegal && got != from:
					t.Fatalf("illegal move returned state %q, want unchanged %q", got, from)
				}
			})
		}
	}
}

func TestFinalStatesAcceptNothing(t *testing.T) {
	for _, s := range States() {
		if !s.Final() {
			continue
		}
		for _, ev := range Events() {
			if _, err := Transition(s, ev); err == nil {
				t.Errorf("final state %q accepted %s", s, ev)
			}
		}
	}
}

func TestOnlyRunningHoldsLease(t *testing.T) {
	for _, s := range States() {
		if got, want := s.HoldsLease(), s == StateRunning; got != want {
			t.Errorf("%q.HoldsLease() = %v, want %v", s, got, want)
		}
	}
}

func name(s State) string {
	if s == StateNone {
		return "none"
	}
	return string(s)
}

// Example functions appear in `go doc` output and are run as tests.
func ExampleTransition() {
	next, err := Transition(StateQueued, EventRunClaimed)
	fmt.Println(next, err)

	_, err = Transition(StateSucceeded, EventRunClaimed)
	fmt.Println(errors.Is(err, ErrIllegalTransition))
	// Output:
	// running <nil>
	// true
}
