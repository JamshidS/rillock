package worker

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jamshids/rillock/internal/runstate"
	"github.com/jamshids/rillock/internal/spec"
	"github.com/jamshids/rillock/internal/store"
	"github.com/jamshids/rillock/internal/testdb"
	"github.com/jamshids/rillock/internal/tools"
)

type env struct {
	store  *store.Store
	worker *Worker
	tools  *httptest.Server
}

// setup gives a migrated database with one tool (served by a test server) and
// two agents: "scripted" follows its input's script, "echo" repeats its input.
func setup(t *testing.T) env {
	t.Helper()
	ctx := t.Context()
	url := testdb.New(t)
	if _, err := store.Migrate(ctx, url); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"amount":300}`)
	}))
	t.Cleanup(srv.Close)

	tool := &spec.Tool{Name: "orders_get", Description: "get an order", Endpoint: srv.URL, Effect: spec.EffectReadOnly}
	if err := tool.Normalize(); err != nil {
		t.Fatal(err)
	}
	resources := []spec.Resource{{Tool: tool}}
	for _, name := range []string{"scripted", "echo"} {
		a := &spec.Agent{Name: name, Model: spec.Model{Provider: "fake", Name: name}, Instructions: "help", Tools: []string{"orders_get"}}
		a.Normalize()
		resources = append(resources, spec.Resource{Agent: a})
	}
	if _, err := st.Apply(ctx, resources); err != nil {
		t.Fatal(err)
	}

	w := New(st, tools.NewHTTPExecutor(tools.DefaultTimeout), slog.New(slog.DiscardHandler), Config{ID: "worker-1", Poll: 10 * time.Millisecond})
	return env{store: st, worker: w, tools: srv}
}

func (e env) submit(t *testing.T, agent, input string) string {
	t.Helper()
	raw, _ := json.Marshal(input)
	run, err := e.store.SubmitRun(t.Context(), agent, raw)
	if err != nil {
		t.Fatal(err)
	}
	return run.ID
}

func (e env) runOnce(t *testing.T) {
	t.Helper()
	worked, err := e.worker.RunOnce(t.Context())
	if err != nil || !worked {
		t.Fatalf("RunOnce = %v, %v; want a run executed without error", worked, err)
	}
}

func (e env) get(t *testing.T, id string) store.Run {
	t.Helper()
	run, err := e.store.GetRun(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func TestRunSucceedsEndToEnd(t *testing.T) {
	e := setup(t)
	id := e.submit(t, "scripted", `{"script": [
		{"toolCalls": [{"name": "orders_get", "input": {"order_id": "ord_812"}}]},
		{"text": "Refunded $300."}
	]}`)

	e.runOnce(t)

	run := e.get(t, id)
	if run.State != runstate.StateSucceeded || string(run.Result) != `"Refunded $300."` {
		t.Fatalf("run = %s, result %s", run.State, run.Result)
	}
	if run.StepsUsed != 3 || run.InputTokens == 0 {
		t.Fatalf("run used %d steps and %d input tokens", run.StepsUsed, run.InputTokens)
	}

	events, err := e.store.ListEvents(t.Context(), id, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, ev := range events {
		types = append(types, ev.Type)
	}
	want := "RunCreated RunClaimed StepStarted StepCompleted StepStarted StepCompleted StepStarted StepCompleted RunSucceeded"
	if got := strings.Join(types, " "); got != want {
		t.Fatalf("events:\n got %s\nwant %s", got, want)
	}
}

func TestFailedRunRecordsReason(t *testing.T) {
	e := setup(t)
	id := e.submit(t, "scripted", "this is not a script")
	e.runOnce(t)
	run := e.get(t, id)
	if run.State != runstate.StateFailed || !strings.Contains(run.FailureReason, "model call failed") {
		t.Fatalf("run = %s, reason %q", run.State, run.FailureReason)
	}
}

func TestUnavailableModelFailsTheRun(t *testing.T) {
	e := setup(t)
	a := &spec.Agent{Name: "claude", Model: spec.Model{Provider: "anthropic", Name: "claude-x"}, Instructions: "help"}
	a.Normalize()
	if _, err := e.store.Apply(t.Context(), []spec.Resource{{Agent: a}}); err != nil {
		t.Fatal(err)
	}
	id := e.submit(t, "claude", "hi")
	e.runOnce(t)
	if run := e.get(t, id); run.State != runstate.StateFailed || !strings.Contains(run.FailureReason, "not available yet") {
		t.Fatalf("run = %s, reason %q", run.State, run.FailureReason)
	}
}

func TestIdleWorkerFindsNothing(t *testing.T) {
	e := setup(t)
	worked, err := e.worker.RunOnce(t.Context())
	if err != nil || worked {
		t.Fatalf("RunOnce on an empty queue = %v, %v", worked, err)
	}
}

func TestRunProcessesQueueAndStops(t *testing.T) {
	e := setup(t)
	first := e.submit(t, "echo", "one")
	second := e.submit(t, "echo", "two")

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		e.worker.Run(ctx)
		close(done)
	}()

	deadline := time.Now().Add(10 * time.Second)
	for e.get(t, first).State != runstate.StateSucceeded || e.get(t, second).State != runstate.StateSucceeded {
		if time.Now().After(deadline) {
			t.Fatal("worker did not finish both runs")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := string(e.get(t, second).Result); got != `"echo: two"` {
		t.Fatalf("result = %s", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop after its context was cancelled")
	}
}
