package store

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/jamshids/rillock/internal/runstate"
	"github.com/jamshids/rillock/internal/spec"
)

func eventTypes(events []Event) []string {
	types := make([]string, len(events))
	for i, e := range events {
		types[i] = e.Type
	}
	return types
}

func mustClaim(t *testing.T, s *Store) *ClaimedRun {
	t.Helper()
	c, err := s.ClaimNextRun(t.Context(), "worker-test")
	if err != nil {
		t.Fatalf("ClaimNextRun: %v", err)
	}
	if c == nil {
		t.Fatal("ClaimNextRun found no run")
	}
	return c
}

func TestClaimNextRun(t *testing.T) {
	s, _ := newStore(t)
	ctx := t.Context()

	if c, err := s.ClaimNextRun(ctx, "w"); err != nil || c != nil {
		t.Fatalf("empty queue: got %v, %v; want nil, nil", c, err)
	}

	mustApply(t, s, spec.Resource{Tool: tool("orders_get")}, spec.Resource{Agent: agent("refund", "orders_get")})
	first := mustSubmit(ctx, t, s, "refund")
	mustSubmit(ctx, t, s, "refund")

	c := mustClaim(t, s)
	if c.Run.ID != first.ID || c.Run.State != runstate.StateRunning {
		t.Fatalf("claimed %s in %s, want the oldest run %s in running", c.Run.ID, c.Run.State, first.ID)
	}
	if c.Agent.Name != "refund" || len(c.Tools) != 1 || c.Tools[0].Name != "orders_get" {
		t.Fatalf("claimed run has agent %q and tools %v", c.Agent.Name, c.Tools)
	}
	got := eventTypes(mustListEvents(ctx, t, s, first.ID))
	if len(got) != 2 || got[1] != string(runstate.EventRunClaimed) {
		t.Fatalf("events = %v, want RunCreated, RunClaimed", got)
	}
}

func TestConcurrentClaimsNeverShareARun(t *testing.T) {
	s, _ := newStore(t)
	ctx := t.Context()
	mustApply(t, s, spec.Resource{Agent: agent("refund")})
	for range 5 {
		mustSubmit(ctx, t, s, "refund")
	}

	var mu sync.Mutex
	claimedBy := map[string]int{}
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			c, err := s.ClaimNextRun(ctx, "w")
			if err != nil {
				t.Error(err)
				return
			}
			if c != nil {
				mu.Lock()
				claimedBy[c.Run.ID]++
				mu.Unlock()
			}
		})
	}
	wg.Wait()

	if len(claimedBy) != 5 {
		t.Fatalf("10 workers claimed %d distinct runs, want all 5", len(claimedBy))
	}
	for id, n := range claimedBy {
		if n != 1 {
			t.Fatalf("run %s was claimed %d times", id, n)
		}
	}
}

func TestStepsRecordUsageAndEvents(t *testing.T) {
	s, _ := newStore(t)
	ctx := t.Context()
	mustApply(t, s, spec.Resource{Tool: tool("orders_get")}, spec.Resource{Agent: agent("refund", "orders_get")})
	mustSubmit(ctx, t, s, "refund")
	run := mustClaim(t, s).Run

	seq, err := s.StartStep(ctx, run.ID, StepStart{Kind: StepModel, Request: json.RawMessage(`{}`)})
	if err != nil || seq != 1 {
		t.Fatalf("StartStep = %d, %v; want 1", seq, err)
	}
	if err := s.FinishStep(ctx, run.ID, seq, StepFinish{
		Status: StepCompleted, Result: json.RawMessage(`{"text":"hi"}`),
		InputTokens: 10, OutputTokens: 5, CostMicroUSD: 7,
	}); err != nil {
		t.Fatalf("FinishStep: %v", err)
	}

	seq, err = s.StartStep(ctx, run.ID, StepStart{
		Kind: StepTool, ToolName: "orders_get", ToolUseID: "call_1", Request: json.RawMessage(`{"id":1}`),
	})
	if err != nil || seq != 2 {
		t.Fatalf("second StartStep = %d, %v; want 2", seq, err)
	}
	if err := s.FinishStep(ctx, run.ID, seq, StepFinish{Status: StepFailed, Error: "404"}); err != nil {
		t.Fatalf("FinishStep: %v", err)
	}

	// A step can finish only once.
	if err := s.FinishStep(ctx, run.ID, seq, StepFinish{Status: StepCompleted}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("finishing twice: err = %v, want ErrNotFound", err)
	}

	steps, err := s.ListSteps(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 2 || steps[1].ToolName != "orders_get" || steps[1].Status != StepFailed ||
		steps[1].IdempotencyKey != run.ID+":2" {
		t.Fatalf("steps = %+v", steps)
	}

	got, err := s.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.StepsUsed != 2 || got.InputTokens != 10 || got.OutputTokens != 5 || got.CostMicroUSD != 7 {
		t.Fatalf("run totals = %d steps, %d/%d tokens, %d cost", got.StepsUsed, got.InputTokens, got.OutputTokens, got.CostMicroUSD)
	}

	want := []string{"RunCreated", "RunClaimed", "StepStarted", "StepCompleted", "StepStarted", "StepFailed"}
	if types := eventTypes(mustListEvents(ctx, t, s, run.ID)); len(types) != len(want) {
		t.Fatalf("events = %v, want %v", types, want)
	} else {
		for i := range want {
			if types[i] != want[i] {
				t.Fatalf("events = %v, want %v", types, want)
			}
		}
	}
}

func TestStepsNeedARunningRun(t *testing.T) {
	s, _ := newStore(t)
	ctx := t.Context()
	mustApply(t, s, spec.Resource{Agent: agent("refund")})
	run := mustSubmit(ctx, t, s, "refund") // queued, not running

	if _, err := s.StartStep(ctx, run.ID, StepStart{Kind: StepModel, Request: json.RawMessage(`{}`)}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("StartStep on a queued run: err = %v, want ErrNotFound", err)
	}
}

func TestCompleteAndFailRun(t *testing.T) {
	s, _ := newStore(t)
	ctx := t.Context()
	mustApply(t, s, spec.Resource{Agent: agent("refund")})

	mustSubmit(ctx, t, s, "refund")
	done := mustClaim(t, s).Run
	if err := s.CompleteRun(ctx, done.ID, json.RawMessage(`"all good"`)); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetRun(ctx, done.ID)
	if got.State != runstate.StateSucceeded || string(got.Result) != `"all good"` {
		t.Fatalf("run = %s with result %s", got.State, got.Result)
	}
	// A finished run cannot finish again.
	if err := s.FailRun(ctx, done.ID, "late"); !errors.Is(err, runstate.ErrIllegalTransition) {
		t.Fatalf("failing a succeeded run: err = %v", err)
	}

	mustSubmit(ctx, t, s, "refund")
	broken := mustClaim(t, s).Run
	if err := s.FailRun(ctx, broken.ID, "step limit reached"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetRun(ctx, broken.ID)
	if got.State != runstate.StateFailed || got.FailureReason != "step limit reached" {
		t.Fatalf("run = %s, reason %q", got.State, got.FailureReason)
	}
}
