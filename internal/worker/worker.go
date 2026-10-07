// Package worker claims queued runs and executes them.
//
// A worker repeats: claim the oldest queued run, run the agent loop on it,
// record the outcome. When no run is waiting it sleeps for its poll interval.
// Phase D adds leases and heartbeats, so that runs held by a dead worker are
// picked up by another.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jamshids/rillock/internal/loop"
	"github.com/jamshids/rillock/internal/model"
	"github.com/jamshids/rillock/internal/spec"
	"github.com/jamshids/rillock/internal/store"
)

// DefaultPoll is how often an idle worker checks for queued runs.
const DefaultPoll = time.Second

// Worker executes runs, one at a time.
type Worker struct {
	id       string
	store    *store.Store
	exec     loop.ToolExecutor
	newModel func(spec.Model) (model.Provider, error)
	poll     time.Duration
	log      *slog.Logger
}

// Config configures a worker. Zero fields get defaults.
type Config struct {
	ID       string                                   // default: hostname/pid
	Poll     time.Duration                            // default: DefaultPoll
	NewModel func(spec.Model) (model.Provider, error) // default: model.New
}

// New returns a worker that records runs in st and calls tools with exec.
func New(st *store.Store, exec loop.ToolExecutor, log *slog.Logger, cfg Config) *Worker {
	w := &Worker{id: cfg.ID, store: st, exec: exec, newModel: cfg.NewModel, poll: cfg.Poll, log: log}
	if w.id == "" {
		host, _ := os.Hostname() // an empty hostname still gives a usable ID
		w.id = fmt.Sprintf("%s/%d", host, os.Getpid())
	}
	if w.poll == 0 {
		w.poll = DefaultPoll
	}
	if w.newModel == nil {
		w.newModel = model.New
	}
	w.log = log.With("worker", w.id)
	return w
}

// ID identifies the worker in events such as RunClaimed.
func (w *Worker) ID() string { return w.id }

// Run executes runs until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	w.log.Info("worker started")
	for {
		worked, err := w.RunOnce(ctx)
		if ctx.Err() != nil {
			w.log.Info("worker stopped")
			return
		}
		if err != nil {
			w.log.Error("worker error", "err", err)
		}
		if worked && err == nil {
			continue // there may be more waiting: look again right away
		}
		select {
		case <-ctx.Done():
			w.log.Info("worker stopped")
			return
		case <-time.After(w.poll):
		}
	}
}

// RunOnce claims one queued run and executes it to the end. It reports
// whether there was a run to execute.
//
// An error means the outcome could not be recorded (for example, the
// database went away). The run is left running; Phase D recovers such runs.
func (w *Worker) RunOnce(ctx context.Context) (bool, error) {
	claimed, err := w.store.ClaimNextRun(ctx, w.id)
	if err != nil || claimed == nil {
		return false, err
	}
	run := claimed.Run
	log := w.log.With("run", run.ID, "agent", run.Agent)
	log.Info("run claimed")

	provider, err := w.newModel(claimed.Agent.Model)
	if err != nil {
		return true, w.fail(ctx, log, run.ID, err.Error())
	}

	outcome, err := loop.Execute(ctx, loop.Run{
		ID:    run.ID,
		Agent: claimed.Agent,
		Tools: claimed.Tools,
		Input: run.Input,
	}, provider, w.exec, journal{store: w.store, runID: run.ID})
	if err != nil {
		if errors.Is(err, context.Canceled) {
			log.Warn("stopped in the middle of a run; it stays running until recovered")
		}
		return true, fmt.Errorf("run %s: %w", run.ID, err)
	}

	if outcome.Failure != "" {
		return true, w.fail(ctx, log, run.ID, outcome.Failure)
	}
	answer, err := json.Marshal(outcome.Answer)
	if err != nil {
		return true, err
	}
	if err := w.store.CompleteRun(ctx, run.ID, answer); err != nil {
		return true, fmt.Errorf("run %s: record success: %w", run.ID, err)
	}
	log.Info("run succeeded")
	return true, nil
}

func (w *Worker) fail(ctx context.Context, log *slog.Logger, runID, reason string) error {
	if err := w.store.FailRun(ctx, runID, reason); err != nil {
		return fmt.Errorf("run %s: record failure: %w", runID, err)
	}
	log.Info("run failed", "reason", reason)
	return nil
}

// journal connects the loop to the store for one run.
type journal struct {
	store *store.Store
	runID string
}

func (j journal) StartStep(ctx context.Context, st store.StepStart) (int, error) {
	return j.store.StartStep(ctx, j.runID, st)
}

func (j journal) FinishStep(ctx context.Context, seq int, f store.StepFinish) error {
	return j.store.FinishStep(ctx, j.runID, seq, f)
}
