package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// Fake is a model that needs no network or API key, for tests and offline
// demos. Two behaviors are available, chosen by the model name:
//
//   - "echo" answers with the task text.
//
//   - "scripted" follows a script given as the run input:
//
//     {"script": [
//     {"toolCalls": [{"name": "orders_get", "input": {"order_id": "ord_812"}}]},
//     {"text": "Order ord_812 was refunded."}
//     ]}
//
// Each entry is one model reply. Add "repeatLast": true to repeat the last
// entry forever, which simulates a model that never finishes.
//
// Fake is stateless: it finds its place in the script by counting the
// assistant messages in the conversation. A run resumed after a crash
// therefore continues the script where it left off.
type Fake struct {
	behavior string
}

// Fake behaviors.
const (
	FakeEcho     = "echo"
	FakeScripted = "scripted"
)

// NewFake returns a fake model with the named behavior.
func NewFake(behavior string) (*Fake, error) {
	if behavior != FakeEcho && behavior != FakeScripted {
		return nil, fmt.Errorf("unknown fake model %q (want %q or %q)", behavior, FakeEcho, FakeScripted)
	}
	return &Fake{behavior: behavior}, nil
}

type script struct {
	Script     []scriptTurn `json:"script"`
	RepeatLast bool         `json:"repeatLast"`
}

type scriptTurn struct {
	Text      string       `json:"text"`
	ToolCalls []scriptCall `json:"toolCalls"`
}

type scriptCall struct {
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// Complete implements Provider.
func (f *Fake) Complete(ctx context.Context, req Request) (Response, error) {
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	task := firstUserText(req.Messages)

	var resp Response
	switch f.behavior {
	case FakeEcho:
		resp = textResponse("echo: " + task)
	case FakeScripted:
		var err error
		resp, err = scriptedResponse(task, countAssistant(req.Messages))
		if err != nil {
			return Response{}, err
		}
	}
	resp.Usage = fakeUsage(req, resp)
	return resp, nil
}

func scriptedResponse(task string, turn int) (Response, error) {
	var s script
	if err := json.Unmarshal([]byte(task), &s); err != nil || len(s.Script) == 0 {
		return Response{}, errors.New(`scripted fake model: the run input must be {"script": [...]} with at least one entry`)
	}
	switch {
	case turn < len(s.Script):
	case s.RepeatLast:
		turn = len(s.Script) - 1
	default:
		return textResponse("(script finished)"), nil
	}

	entry := s.Script[turn]
	if len(entry.ToolCalls) == 0 {
		return textResponse(entry.Text), nil
	}
	resp := Response{StopReason: StopToolUse}
	if entry.Text != "" {
		resp.Content = append(resp.Content, Block{Type: BlockText, Text: entry.Text})
	}
	for i, call := range entry.ToolCalls {
		input := call.Input
		if len(input) == 0 {
			input = json.RawMessage(`{}`)
		}
		resp.Content = append(resp.Content, Block{
			Type: BlockToolUse,
			// Deterministic IDs: the same turn always produces the same calls.
			ToolUseID: fmt.Sprintf("call_%d_%d", turn+1, i+1),
			Name:      call.Name,
			Input:     input,
		})
	}
	return resp, nil
}

func textResponse(text string) Response {
	return Response{Content: []Block{{Type: BlockText, Text: text}}, StopReason: StopEndTurn}
}

func firstUserText(msgs []Message) string {
	for _, m := range msgs {
		if m.Role != RoleUser {
			continue
		}
		for _, b := range m.Content {
			if b.Type == BlockText {
				return b.Text
			}
		}
	}
	return ""
}

func countAssistant(msgs []Message) int {
	n := 0
	for _, m := range msgs {
		if m.Role == RoleAssistant {
			n++
		}
	}
	return n
}

// fakeUsage estimates tokens as characters / 4, a common rule of thumb, so
// traces show plausible numbers. A fake model costs nothing.
func fakeUsage(req Request, resp Response) Usage {
	in := len(req.System)
	for _, m := range req.Messages {
		for _, b := range m.Content {
			in += len(b.Text) + len(b.Input)
		}
	}
	out := 0
	for _, b := range resp.Content {
		out += len(b.Text) + len(b.Input)
	}
	return Usage{InputTokens: int64(in/4 + 1), OutputTokens: int64(out/4 + 1)}
}
