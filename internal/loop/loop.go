// Package loop runs an agent: it calls the model, runs the tools the model
// asks for, sends the results back, and repeats until the model answers.
//
//	model ──► tool calls? ──no──► answer: done
//	  ▲            │ yes
//	  │            ▼
//	  └──── run each tool, record each result
//
// Every model call and every tool call is a step. A step is recorded in the
// journal before it starts and again when it ends, so the journal always shows
// what was attempted, even if the process dies in between.
//
// The loop decides what the agent may do; the model only proposes. A tool the
// agent was not given is refused here, in code (invariant 5), whatever the
// model says.
package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jamshids/rillock/internal/model"
	"github.com/jamshids/rillock/internal/spec"
	"github.com/jamshids/rillock/internal/store"
	"github.com/jamshids/rillock/internal/tools"
)

// Journal records steps. The worker's implementation writes them to the store.
type Journal interface {
	StartStep(ctx context.Context, st store.StepStart) (seq int, err error)
	FinishStep(ctx context.Context, seq int, f store.StepFinish) error
}

// ToolExecutor calls tools. *tools.HTTPExecutor satisfies it.
type ToolExecutor interface {
	Execute(ctx context.Context, c tools.Call) (tools.Result, error)
}

// Run is what the loop needs to know about one run.
type Run struct {
	ID    string
	Agent spec.Agent
	Tools []spec.Tool
	Input json.RawMessage
}

// Outcome is how a run ended: with an answer, or with a failure reason.
// Exactly one of the two is set.
type Outcome struct {
	Answer  string
	Failure string
}

// maxTokens caps each model reply.
const maxTokens = 4096

// summaryLength caps the one-line summaries written to the event log.
const summaryLength = 200

// Execute runs the agent until it answers or fails.
//
// A problem with the run itself (the model errors, a tool is unreachable, the
// step limit is reached) is an Outcome with Failure set. A returned error means
// the journal could not be written, so the outcome cannot be recorded either;
// the run stays running and is recovered later (Phase D).
func Execute(ctx context.Context, run Run, provider model.Provider, exec ToolExecutor, journal Journal) (Outcome, error) {
	l := &loop{run: run, provider: provider, exec: exec, journal: journal, allowed: map[string]spec.Tool{}}
	for _, t := range run.Tools {
		l.allowed[t.Name] = t
	}

	answer, err := l.execute(ctx)
	var stop *stopError
	switch {
	case errors.As(err, &stop):
		return Outcome{Failure: stop.reason}, nil
	case err != nil:
		return Outcome{}, err
	default:
		return Outcome{Answer: answer}, nil
	}
}

// stopError ends the run with a failure reason. It travels as an error so each
// step function has a single error path; Execute turns it into an Outcome.
type stopError struct {
	reason string
}

func (e *stopError) Error() string { return e.reason }

func stopRun(format string, args ...any) error {
	return &stopError{reason: fmt.Sprintf(format, args...)}
}

type loop struct {
	run      Run
	provider model.Provider
	exec     ToolExecutor
	journal  Journal
	allowed  map[string]spec.Tool
	steps    int
	messages []model.Message
}

// execute returns the model's final answer.
func (l *loop) execute(ctx context.Context) (string, error) {
	l.messages = []model.Message{{
		Role:    model.RoleUser,
		Content: []model.Block{{Type: model.BlockText, Text: inputText(l.run.Input)}},
	}}

	for {
		resp, err := l.callModel(ctx)
		if err != nil {
			return "", err
		}
		l.messages = append(l.messages, model.Message{Role: model.RoleAssistant, Content: resp.Content})

		calls := resp.ToolCalls()
		if len(calls) == 0 {
			if resp.StopReason == model.StopMaxTokens {
				return "", stopRun("the model's reply was cut off at the token limit")
			}
			return resp.Text(), nil
		}

		// The model may ask for several tools at once. They run in the order
		// written, each as its own step, and all results go back together.
		results := make([]model.Block, 0, len(calls))
		for _, call := range calls {
			result, err := l.callTool(ctx, call)
			if err != nil {
				return "", err
			}
			results = append(results, result)
		}
		l.messages = append(l.messages, model.Message{Role: model.RoleUser, Content: results})
	}
}

// startStep checks the step limit and records the start of a step.
func (l *loop) startStep(ctx context.Context, st store.StepStart) (int, error) {
	if l.steps >= l.run.Agent.Limits.MaxSteps {
		return 0, stopRun("step limit reached (%d steps)", l.run.Agent.Limits.MaxSteps)
	}
	seq, err := l.journal.StartStep(ctx, st)
	if err != nil {
		return 0, err
	}
	l.steps++
	return seq, nil
}

// callModel runs one model step.
func (l *loop) callModel(ctx context.Context) (model.Response, error) {
	req := model.Request{
		Model:     l.run.Agent.Model.Name,
		System:    l.run.Agent.Instructions,
		Messages:  l.messages,
		Tools:     l.toolDefs(),
		MaxTokens: maxTokens,
	}
	// The step records the model and the conversation length, not the whole
	// conversation: that is rebuilt from earlier steps, and storing it again
	// on every step would grow the journal quadratically.
	request, err := json.Marshal(map[string]any{"model": req.Model, "messages": len(req.Messages)})
	if err != nil {
		return model.Response{}, err
	}
	seq, err := l.startStep(ctx, store.StepStart{Kind: store.StepModel, Request: request})
	if err != nil {
		return model.Response{}, err
	}

	resp, callErr := l.provider.Complete(ctx, req)
	if callErr != nil {
		if err := l.finish(ctx, seq, store.StepFinish{Status: store.StepFailed, Error: callErr.Error()}); err != nil {
			return model.Response{}, err
		}
		return model.Response{}, stopRun("model call failed: %v", callErr)
	}

	result, err := json.Marshal(resp)
	if err != nil {
		return model.Response{}, err
	}
	return resp, l.finish(ctx, seq, store.StepFinish{
		Status:       store.StepCompleted,
		Result:       result,
		Summary:      modelSummary(resp),
		InputTokens:  resp.Usage.InputTokens,
		OutputTokens: resp.Usage.OutputTokens,
		CostMicroUSD: resp.Usage.CostMicroUSD,
	})
}

// callTool runs one tool step and returns the tool_result block for the model.
func (l *loop) callTool(ctx context.Context, call model.Block) (model.Block, error) {
	seq, err := l.startStep(ctx, store.StepStart{
		Kind: store.StepTool, ToolName: call.Name, ToolUseID: call.ToolUseID, Request: call.Input,
	})
	if err != nil {
		return model.Block{}, err
	}

	tool, ok := l.allowed[call.Name]
	if !ok {
		// Refused in code: the model cannot talk its way into a tool it was
		// not given. The model is told, and may choose another path.
		reason := fmt.Sprintf("tool %q is not available to this agent", call.Name)
		err := l.finish(ctx, seq, store.StepFinish{Status: store.StepDenied, Error: reason})
		return toolResult(call, reason, true), err
	}

	res, callErr := l.exec.Execute(ctx, tools.Call{
		Tool: tool, Input: call.Input, RunID: l.run.ID, Step: seq,
		IdempotencyKey: store.IdempotencyKey(l.run.ID, seq),
	})
	if callErr != nil {
		// No answer at all (unreachable, timeout): the run cannot continue.
		// Retrying safely depends on the tool's effect class (Phase F).
		if err := l.finish(ctx, seq, store.StepFinish{Status: store.StepFailed, Error: callErr.Error()}); err != nil {
			return model.Block{}, err
		}
		return model.Block{}, stopRun("%v", callErr)
	}

	result, err := json.Marshal(res)
	if err != nil {
		return model.Block{}, err
	}
	finish := store.StepFinish{
		Status:  store.StepCompleted,
		Result:  result,
		Summary: fmt.Sprintf("%d %s", res.Status, truncate(string(res.Body))),
	}
	if res.IsError {
		// The tool answered with an error. The run goes on: the model sees the
		// error and may try something else.
		finish.Status = store.StepFailed
		finish.Error = fmt.Sprintf("the tool answered with status %d", res.Status)
	}
	if err := l.finish(ctx, seq, finish); err != nil {
		return model.Block{}, err
	}
	// The tool's answer goes back to the model as plain data (invariant 7).
	return toolResult(call, string(res.Body), res.IsError), nil
}

func (l *loop) finish(ctx context.Context, seq int, f store.StepFinish) error {
	if f.Summary == "" {
		f.Summary = truncate(f.Error)
	}
	return l.journal.FinishStep(ctx, seq, f)
}

func (l *loop) toolDefs() []model.Tool {
	defs := make([]model.Tool, len(l.run.Tools))
	for i, t := range l.run.Tools {
		defs[i] = model.Tool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema}
	}
	return defs
}

func toolResult(call model.Block, text string, isError bool) model.Block {
	return model.Block{Type: model.BlockToolResult, ToolUseID: call.ToolUseID, Text: text, IsError: isError}
}

// inputText turns the run input into the first message. A JSON string becomes
// its text; anything else is passed as JSON.
func inputText(input json.RawMessage) string {
	var s string
	if json.Unmarshal(input, &s) == nil {
		return s
	}
	return string(input)
}

func modelSummary(resp model.Response) string {
	if calls := resp.ToolCalls(); len(calls) > 0 {
		names := make([]string, len(calls))
		for i, c := range calls {
			names[i] = c.Name
		}
		return "→ " + strings.Join(names, ", ")
	}
	return truncate(resp.Text())
}

// truncate shortens s to summaryLength characters for the event log. It cuts
// at a character boundary, never in the middle of a multi-byte character.
func truncate(s string) string {
	s = strings.Join(strings.Fields(s), " ") // one line
	if utf8.RuneCountInString(s) <= summaryLength {
		return s
	}
	runes := []rune(s)
	return string(runes[:summaryLength]) + "…"
}
