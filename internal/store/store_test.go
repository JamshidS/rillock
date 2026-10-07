package store

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/jamshids/rillock/internal/runstate"
	"github.com/jamshids/rillock/internal/spec"
	"github.com/jamshids/rillock/internal/testdb"
)

// newStore returns a Store on a fresh, migrated database.
func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	ctx := t.Context()
	url := testdb.New(t)
	if _, err := Migrate(ctx, url); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	s, err := Open(ctx, url)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(s.Close)
	return s, url
}

func tool(name string) *spec.Tool {
	t := &spec.Tool{
		Name:        name,
		Description: "test tool",
		Endpoint:    "http://tools.test/" + name,
		Effect:      spec.EffectReadOnly,
	}
	if err := t.Normalize(); err != nil {
		panic(err)
	}
	return t
}

func agent(name string, tools ...string) *spec.Agent {
	a := &spec.Agent{
		Name:         name,
		Model:        spec.Model{Provider: "fake", Name: "scripted"},
		Instructions: "test agent",
		Tools:        tools,
	}
	a.Normalize()
	return a
}

func mustApply(t *testing.T, s *Store, resources ...spec.Resource) []ApplyResult {
	t.Helper()
	results, err := s.Apply(t.Context(), resources)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return results
}

func mustSubmit(ctx context.Context, t *testing.T, s *Store, agentName string) Run {
	t.Helper()
	run, err := s.SubmitRun(ctx, agentName, nil)
	if err != nil {
		t.Fatalf("SubmitRun: %v", err)
	}
	return run
}

func mustListEvents(ctx context.Context, t *testing.T, s *Store, runID string) []Event {
	t.Helper()
	events, err := s.ListEvents(ctx, runID, 0, 0)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	return events
}

func TestSubmitRunWithoutInputStoresNull(t *testing.T) {
	s, _ := newStore(t)
	mustApply(t, s, spec.Resource{Agent: agent("refund")})
	if run := mustSubmit(t.Context(), t, s, "refund"); string(run.Input) != "null" {
		t.Fatalf("input = %s, want null", run.Input)
	}
}

func TestApplyVersions(t *testing.T) {
	s, _ := newStore(t)

	first := mustApply(t, s, spec.Resource{Tool: tool("orders_get")}, spec.Resource{Agent: agent("refund", "orders_get")})
	for _, r := range first {
		if r.Status != ApplyCreated || r.Version != 1 {
			t.Errorf("first apply of %s: %+v, want created v1", r.Name, r)
		}
	}

	again := mustApply(t, s, spec.Resource{Agent: agent("refund", "orders_get")})
	if again[0].Status != ApplyUnchanged || again[0].Version != 1 {
		t.Errorf("same spec again: %+v, want unchanged v1", again[0])
	}

	changed := agent("refund", "orders_get")
	changed.Instructions = "new instructions"
	third := mustApply(t, s, spec.Resource{Agent: changed})
	if third[0].Status != ApplyUpdated || third[0].Version != 2 {
		t.Errorf("changed spec: %+v, want updated v2", third[0])
	}
}

func TestApplyRejectsUnknownToolAndChangesNothing(t *testing.T) {
	s, _ := newStore(t)

	_, err := s.Apply(t.Context(), []spec.Resource{
		{Tool: tool("orders_get")},
		{Agent: agent("refund", "orders_get", "payments_refund")},
	})
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *spec.ValidationError", err)
	}

	// The whole request is one transaction: the valid tool was rolled back too.
	if _, err := s.SubmitRun(t.Context(), "refund", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("agent exists after a failed apply: %v", err)
	}
	results := mustApply(t, s, spec.Resource{Tool: tool("orders_get")})
	if results[0].Status != ApplyCreated {
		t.Fatalf("tool exists after a failed apply: %+v", results[0])
	}
}

func TestConcurrentApplyCreatesOneVersion(t *testing.T) {
	s, _ := newStore(t)

	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for range 10 {
		wg.Go(func() {
			_, err := s.Apply(t.Context(), []spec.Resource{{Tool: tool("orders_get")}})
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
	}

	var versions int
	if err := s.pool.QueryRow(t.Context(), `SELECT count(*) FROM tool_versions`).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions != 1 {
		t.Fatalf("10 identical concurrent applies created %d versions, want 1", versions)
	}
}

func TestSubmitRun(t *testing.T) {
	s, _ := newStore(t)
	ctx := t.Context()
	mustApply(t, s, spec.Resource{Tool: tool("orders_get")}, spec.Resource{Agent: agent("refund", "orders_get")})

	input := json.RawMessage(`"refund order 812"`)
	run, err := s.SubmitRun(ctx, "refund", input)
	if err != nil {
		t.Fatalf("SubmitRun: %v", err)
	}
	if run.State != runstate.StateQueued || run.Agent != "refund" || run.AgentVersion != 1 {
		t.Fatalf("run = %+v, want queued refund v1", run)
	}
	if string(run.Input) != string(input) {
		t.Fatalf("input = %s, want %s", run.Input, input)
	}

	events, err := s.ListEvents(ctx, run.ID, 0, 0)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 || events[0].Seq != 1 || events[0].Type != string(runstate.EventRunCreated) {
		t.Fatalf("events = %+v, want exactly RunCreated as event 1", events)
	}

	var pinned int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM run_tool_versions WHERE run_id = $1`, run.ID).
		Scan(&pinned); err != nil {
		t.Fatal(err)
	}
	if pinned != 1 {
		t.Fatalf("pinned %d tool versions, want 1", pinned)
	}
}

func TestSubmitRunUnknownAgent(t *testing.T) {
	s, _ := newStore(t)
	if _, err := s.SubmitRun(t.Context(), "nobody", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// TestFailedEventLeavesNoRun is the Phase B drill: if the event cannot be
// written, the run must not exist either (invariant 1).
func TestFailedEventLeavesNoRun(t *testing.T) {
	s, url := newStore(t)
	ctx := t.Context()
	mustApply(t, s, spec.Resource{Agent: agent("refund")})

	// Sabotage: make every event insert fail.
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	mustExec(t, conn, `
		CREATE FUNCTION fail_event() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'sabotaged event insert'; END $$`)
	mustExec(t, conn, `CREATE TRIGGER sabotage BEFORE INSERT ON events FOR EACH ROW EXECUTE FUNCTION fail_event()`)

	if _, err := s.SubmitRun(ctx, "refund", nil); err == nil {
		t.Fatal("SubmitRun succeeded although the event insert failed")
	}

	var runs int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM runs`).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs != 0 {
		t.Fatalf("found %d runs after a failed event insert, want 0", runs)
	}
}

func TestCancelRun(t *testing.T) {
	s, _ := newStore(t)
	ctx := t.Context()
	mustApply(t, s, spec.Resource{Agent: agent("refund")})

	t.Run("queued run is cancelled immediately", func(t *testing.T) {
		run := mustSubmit(ctx, t, s, "refund")
		got, err := s.CancelRun(ctx, run.ID, "tester")
		if err != nil {
			t.Fatalf("CancelRun: %v", err)
		}
		if got.State != runstate.StateCancelled {
			t.Fatalf("state = %s, want cancelled", got.State)
		}
		events := mustListEvents(ctx, t, s, run.ID)
		if last := events[len(events)-1]; last.Type != string(runstate.EventRunCancelled) || last.Seq != 2 {
			t.Fatalf("last event = %+v, want RunCancelled as event 2", last)
		}
	})

	t.Run("running run only gets the cancel flag", func(t *testing.T) {
		run := mustSubmit(ctx, t, s, "refund")
		// No worker exists until Phase C, so put the run in running directly.
		if _, err := s.pool.Exec(ctx, `UPDATE runs SET state = 'running' WHERE id = $1`, run.ID); err != nil {
			t.Fatal(err)
		}
		got, err := s.CancelRun(ctx, run.ID, "tester")
		if err != nil {
			t.Fatalf("CancelRun: %v", err)
		}
		if got.State != runstate.StateRunning || !got.CancelRequested {
			t.Fatalf("run = state %s, cancel requested %v; want running with the flag set", got.State, got.CancelRequested)
		}

		// Asking again changes nothing and records no second event.
		if _, err := s.CancelRun(ctx, run.ID, "tester"); err != nil {
			t.Fatalf("second CancelRun: %v", err)
		}
		events := mustListEvents(ctx, t, s, run.ID)
		if len(events) != 2 || events[1].Type != EventRunCancelRequested {
			t.Fatalf("events = %+v, want RunCreated then one RunCancelRequested", events)
		}
	})

	t.Run("finished run cannot be cancelled", func(t *testing.T) {
		run := mustSubmit(ctx, t, s, "refund")
		if _, err := s.CancelRun(ctx, run.ID, "tester"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CancelRun(ctx, run.ID, "tester"); !errors.Is(err, runstate.ErrIllegalTransition) {
			t.Fatalf("err = %v, want ErrIllegalTransition", err)
		}
	})

	t.Run("unknown run", func(t *testing.T) {
		if _, err := s.CancelRun(ctx, "01928f3e-0000-7000-8000-000000000000", "tester"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})
}

func TestListRuns(t *testing.T) {
	s, _ := newStore(t)
	ctx := t.Context()
	mustApply(t, s, spec.Resource{Agent: agent("refund")})

	var ids []string
	for range 3 {
		ids = append(ids, mustSubmit(ctx, t, s, "refund").ID)
	}
	if _, err := s.CancelRun(ctx, ids[0], "tester"); err != nil {
		t.Fatal(err)
	}

	all, err := s.ListRuns(ctx, ListRunsFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || all[0].ID != ids[2] {
		t.Fatalf("got %d runs starting with %s, want 3 starting with the newest %s", len(all), all[0].ID, ids[2])
	}

	queued, err := s.ListRuns(ctx, ListRunsFilter{State: runstate.StateQueued})
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 2 {
		t.Fatalf("got %d queued runs, want 2", len(queued))
	}

	limited, err := s.ListRuns(ctx, ListRunsFilter{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 1 {
		t.Fatalf("limit 1 returned %d runs", len(limited))
	}
}

func TestListEventsAfterSeq(t *testing.T) {
	s, _ := newStore(t)
	ctx := t.Context()
	mustApply(t, s, spec.Resource{Agent: agent("refund")})
	run := mustSubmit(ctx, t, s, "refund")
	if _, err := s.CancelRun(ctx, run.ID, "tester"); err != nil {
		t.Fatal(err)
	}

	later, err := s.ListEvents(ctx, run.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(later) != 1 || later[0].Seq != 2 {
		t.Fatalf("events after 1 = %+v, want only event 2", later)
	}

	if _, err := s.ListEvents(ctx, "01928f3e-0000-7000-8000-000000000000", 0, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound for an unknown run", err)
	}
}

func TestListRunsByIDPrefix(t *testing.T) {
	s, _ := newStore(t)
	ctx := t.Context()
	mustApply(t, s, spec.Resource{Agent: agent("refund")})
	oldest := mustSubmit(ctx, t, s, "refund")
	for range 3 {
		mustSubmit(ctx, t, s, "refund")
	}

	// The filter applies before the limit, so the oldest run is found even
	// though a newest-first page of 1 would not include it.
	unique := oldest.ID[:len(oldest.ID)-1]
	got, err := s.ListRuns(ctx, ListRunsFilter{IDPrefix: unique, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != oldest.ID {
		t.Fatalf("prefix %q found %v, want only %s", unique, got, oldest.ID)
	}

	// '%' and '_' are ordinary characters here, not SQL wildcards.
	for _, p := range []string{"%", "_"} {
		runs, err := s.ListRuns(ctx, ListRunsFilter{IDPrefix: p})
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) != 0 {
			t.Fatalf("prefix %q matched %d runs, want 0", p, len(runs))
		}
	}
}
