package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"maps"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jamshids/rillock/internal/apiv1"
)

func runTrace(ctx context.Context, a *app, fs *flag.FlagSet, args []string) error {
	cl, rest, err := parseClient(a, fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usageErrorf("expected exactly one run ID")
	}
	id, err := resolveRunID(ctx, cl, rest[0])
	if err != nil {
		return err
	}
	run, err := cl.GetRun(ctx, id)
	if err != nil {
		return err
	}
	events, err := cl.ListEvents(ctx, id, 0)
	if err != nil {
		return err
	}
	a.printTrace(run, events)
	return nil
}

// eventPayload holds the fields any event may carry. Each event type sets only
// some of them; the rest stay at their zero values.
type eventPayload struct {
	Agent        string          `json:"agent"`
	AgentVersion int             `json:"agentVersion"`
	Worker       string          `json:"worker"`
	By           string          `json:"by"`
	Reason       string          `json:"reason"`
	Seq          int             `json:"seq"`
	Kind         string          `json:"kind"`
	Tool         string          `json:"tool"`
	Input        json.RawMessage `json:"input"`
	Summary      string          `json:"summary"`
	Error        string          `json:"error"`
	DurationMs   int64           `json:"durationMs"`
	InputTokens  int64           `json:"inputTokens"`
	OutputTokens int64           `json:"outputTokens"`
	CostMicroUSD int64           `json:"costMicroUSD"`
}

// printTrace shows a run as a timeline, one line per thing that happened.
// A step's start and end are merged into one line; a step that started but
// never finished (its worker died, or it is still running) says so.
func (a *app) printTrace(run apiv1.Run, events []apiv1.Event) {
	fmt.Fprintf(a.stdout, "Run %s  %s v%d  %s\n", run.ID, run.Agent, run.AgentVersion, run.State)
	fmt.Fprintf(a.stdout, "%d steps, %d+%d tokens, %s, %s\n\n", run.StepsUsed, run.InputTokens, run.OutputTokens,
		formatCost(run.CostMicroUSD), run.UpdatedAt.Sub(run.CreatedAt).Round(time.Millisecond))

	// Columns: time, what, duration, tokens, details. The details column has
	// any width, so it comes last and short lines stay short.
	tw := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
	line := func(at, what, duration, usage, details string) {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", at, what, duration, usage, details)
	}
	started := map[int]eventPayload{} // steps that have started but not finished
	for _, e := range events {
		var p eventPayload
		_ = json.Unmarshal(e.Payload, &p) // unknown or missing fields stay empty; the line still prints
		at := e.CreatedAt.Local().Format("15:04:05.000")

		switch e.Type {
		case "RunCreated":
			line(at, "created", "", "", fmt.Sprintf("%s v%d", p.Agent, p.AgentVersion))
		case "RunClaimed":
			line(at, "claimed", "", "", "by "+p.Worker)
		case "StepStarted":
			started[p.Seq] = p
		case "StepCompleted", "StepFailed", "StepDenied":
			start := started[p.Seq]
			delete(started, p.Seq)
			line(at, fmt.Sprintf("step %d %s", p.Seq, stepMark(e.Type)), stepDuration(p), stepUsage(p), stepDescription(start, p))
		case "RunCancelRequested":
			line(at, "cancel requested", "", "", "by "+p.By)
		case "RunCancelled":
			line(at, "cancelled", "", "", "by "+p.By)
		case "RunFailed":
			line(at, "failed", "", "", p.Reason)
		case "RunSucceeded":
			line(at, "succeeded", "", "", compactJSON(run.Result))
		default:
			line(at, e.Type, "", "", compactJSON(e.Payload))
		}
	}
	// Map iteration order is random in Go; sort so the output is stable.
	for _, seq := range slices.Sorted(maps.Keys(started)) {
		line("", fmt.Sprintf("step %d …", seq), "", "", stepDescription(started[seq], eventPayload{})+" (started, not finished)")
	}
	_ = tw.Flush()
}

// stepMark shows at a glance how a step ended.
func stepMark(eventType string) string {
	switch eventType {
	case "StepFailed":
		return "✗"
	case "StepDenied":
		return "⊘"
	default:
		return "✓"
	}
}

func stepDescription(start, end eventPayload) string {
	kind := start.Kind
	if kind == "" {
		kind = end.Kind
	}
	if kind == "model" {
		return "model " + end.Summary
	}
	tool := start.Tool
	if tool == "" {
		tool = end.Tool
	}
	var b strings.Builder
	b.WriteString(tool)
	if len(start.Input) > 0 {
		b.WriteString(" " + compactJSON(start.Input))
	}
	if end.Summary != "" {
		b.WriteString(" → " + end.Summary)
	}
	return b.String()
}

func stepUsage(p eventPayload) string {
	if p.InputTokens == 0 && p.OutputTokens == 0 {
		return ""
	}
	return fmt.Sprintf("%d+%d tokens %s", p.InputTokens, p.OutputTokens, formatCost(p.CostMicroUSD))
}

func stepDuration(p eventPayload) string {
	return (time.Duration(p.DurationMs) * time.Millisecond).String()
}
