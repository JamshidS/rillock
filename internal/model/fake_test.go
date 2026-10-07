package model

import (
	"context"
	"strings"
	"testing"

	"github.com/jamshids/rillock/internal/spec"
)

func userText(text string) Message {
	return Message{Role: RoleUser, Content: []Block{{Type: BlockText, Text: text}}}
}

func assistant(blocks ...Block) Message {
	return Message{Role: RoleAssistant, Content: blocks}
}

func complete(t *testing.T, p Provider, msgs ...Message) Response {
	t.Helper()
	resp, err := p.Complete(t.Context(), Request{Messages: msgs})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	return resp
}

func TestEcho(t *testing.T) {
	p, err := NewFake(FakeEcho)
	if err != nil {
		t.Fatal(err)
	}
	resp := complete(t, p, userText("hello"))
	if resp.StopReason != StopEndTurn || resp.Text() != "echo: hello" {
		t.Fatalf("got %q (%s)", resp.Text(), resp.StopReason)
	}
	if resp.Usage.InputTokens == 0 || resp.Usage.CostMicroUSD != 0 {
		t.Fatalf("usage = %+v, want some tokens and no cost", resp.Usage)
	}
}

const refundScript = `{"script": [
	{"toolCalls": [
		{"name": "orders_get", "input": {"order_id": "ord_812"}},
		{"name": "customers_get"}
	]},
	{"text": "Refund issued."}
]}`

func TestScriptedFollowsScriptByPosition(t *testing.T) {
	p, _ := NewFake(FakeScripted)

	first := complete(t, p, userText(refundScript))
	calls := first.ToolCalls()
	if first.StopReason != StopToolUse || len(calls) != 2 {
		t.Fatalf("turn 1 = %+v, want two tool calls", first)
	}
	if calls[0].Name != "orders_get" || string(calls[0].Input) != `{"order_id": "ord_812"}` || calls[0].ToolUseID != "call_1_1" {
		t.Fatalf("first call = %+v", calls[0])
	}
	if string(calls[1].Input) != `{}` {
		t.Fatalf("a call without input should get {}, got %s", calls[1].Input)
	}

	// The position comes from the conversation, not from memory: a second,
	// separate Fake gives the same answer for the same history.
	other, _ := NewFake(FakeScripted)
	second := complete(t, other, userText(refundScript), assistant(first.Content...), userText("results"))
	if second.StopReason != StopEndTurn || second.Text() != "Refund issued." {
		t.Fatalf("turn 2 = %q (%s)", second.Text(), second.StopReason)
	}

	third := complete(t, p, userText(refundScript), assistant(), userText("r"), assistant(), userText("r"))
	if third.Text() != "(script finished)" {
		t.Fatalf("after the script: %q", third.Text())
	}
}

func TestScriptedRepeatLast(t *testing.T) {
	p, _ := NewFake(FakeScripted)
	loop := `{"script": [{"toolCalls": [{"name": "orders_get"}]}], "repeatLast": true}`
	for turn := range 5 {
		msgs := []Message{userText(loop)}
		for range turn {
			msgs = append(msgs, assistant(), userText("r"))
		}
		if resp := complete(t, p, msgs...); resp.StopReason != StopToolUse {
			t.Fatalf("turn %d stopped with %s, want another tool call", turn+1, resp.StopReason)
		}
	}
}

func TestScriptedRejectsBadInput(t *testing.T) {
	p, _ := NewFake(FakeScripted)
	for _, input := range []string{"plain text", `{"script": []}`, `{"script": "x"}`} {
		_, err := p.Complete(t.Context(), Request{Messages: []Message{userText(input)}})
		if err == nil || !strings.Contains(err.Error(), "run input must be") {
			t.Errorf("input %q: err = %v", input, err)
		}
	}
}

func TestNew(t *testing.T) {
	if _, err := New(spec.Model{Provider: "fake", Name: "echo"}); err != nil {
		t.Fatalf("fake echo: %v", err)
	}
	for _, m := range []spec.Model{
		{Provider: "fake", Name: "clever"},
		{Provider: "anthropic", Name: "claude"},
		{Provider: "openai", Name: "gpt"},
	} {
		if _, err := New(m); err == nil {
			t.Errorf("New(%+v) succeeded", m)
		}
	}
}

func TestCancelledContext(t *testing.T) {
	p, _ := NewFake(FakeEcho)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := p.Complete(ctx, Request{}); err == nil {
		t.Fatal("Complete ignored a cancelled context")
	}
}
