package spec

import (
	"errors"
	"strings"
	"testing"
)

const validAgent = `{
	"kind": "Agent",
	"name": "refund-agent",
	"model": {"provider": "anthropic", "name": "claude-sonnet-5"},
	"instructions": "You help customers with refunds.",
	"tools": ["payments_refund", "orders_get"]
}`

const validTool = `{
	"kind": "Tool",
	"name": "payments_refund",
	"description": "Refund an order",
	"endpoint": "http://payments:9000/refund",
	"effect": "keyed",
	"inputSchema": {"type": "object", "properties": {"amount": {"type": "number"}}},
	"secrets": ["PAYMENTS_API_KEY"],
	"policy": [{"when": "args.amount > 100", "action": "require_approval"}]
}`

func mustDecode(t *testing.T, data string) Resource {
	t.Helper()
	r, err := Decode([]byte(data))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	return r
}

func TestDecodeValidResources(t *testing.T) {
	agent := mustDecode(t, validAgent)
	if agent.Kind() != KindAgent || agent.Name() != "refund-agent" {
		t.Fatalf("got %s %s, want Agent refund-agent", agent.Kind(), agent.Name())
	}
	if err := agent.Agent.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	tool := mustDecode(t, validTool)
	if tool.Kind() != KindTool || tool.Name() != "payments_refund" {
		t.Fatalf("got %s %s, want Tool payments_refund", tool.Kind(), tool.Name())
	}
	if err := tool.Tool.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestDecodeRejects(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		wantErr string
	}{
		{"not JSON", `{`, "invalid resource"},
		{"missing kind", `{"name": "x"}`, `missing "kind"`},
		{"unknown kind", `{"kind": "Pod"}`, `unknown kind "Pod"`},
		// A typo must fail loudly. Silently ignoring "efect" would store a
		// tool with no effect class.
		{"misspelled field", `{"kind": "Tool", "name": "t", "efect": "keyed"}`, `unknown field "efect"`},
		{"two resources in one", `{"kind": "Tool"} {"kind": "Tool"}`, "invalid resource"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Decode([]byte(tt.data))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestAgentDefaults(t *testing.T) {
	a := mustDecode(t, validAgent).Agent
	if a.Limits.MaxSteps != DefaultMaxSteps || a.Limits.MaxCostUSD != DefaultMaxCostUSD ||
		a.Limits.MaxDuration != DefaultMaxDuration {
		t.Fatalf("limits = %+v, want the defaults", a.Limits)
	}
	if strings.Join(a.Tools, ",") != "orders_get,payments_refund" {
		t.Fatalf("tools = %v, want them sorted", a.Tools)
	}
}

func TestAgentValidation(t *testing.T) {
	tests := []struct {
		name    string
		edit    func(a *Agent)
		wantErr string
	}{
		{"bad name", func(a *Agent) { a.Name = "Refund Agent" }, "name:"},
		{"unknown provider", func(a *Agent) { a.Model.Provider = "openai" }, "model.provider"},
		{"no model name", func(a *Agent) { a.Model.Name = "" }, "model.name"},
		{"no instructions", func(a *Agent) { a.Instructions = "" }, "instructions: required"},
		{"bad tool name", func(a *Agent) { a.Tools = []string{"payments.refund"} }, "not a valid tool name"},
		{"duplicate tool", func(a *Agent) { a.Tools = []string{"a", "a"} }, "listed twice"},
		{"too many steps", func(a *Agent) { a.Limits.MaxSteps = 5000 }, "limits.maxSteps"},
		{"negative cost", func(a *Agent) { a.Limits.MaxCostUSD = -1 }, "limits.maxCostUSD"},
		{"bad duration", func(a *Agent) { a.Limits.MaxDuration = "soon" }, "limits.maxDuration"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := mustDecode(t, validAgent).Agent
			tt.edit(a)
			assertValidationError(t, a.Validate(), tt.wantErr)
		})
	}
}

func TestToolValidation(t *testing.T) {
	tests := []struct {
		name    string
		edit    func(tool *Tool)
		wantErr string
	}{
		{"dotted name", func(tool *Tool) { tool.Name = "payments.refund" }, "name:"},
		{"no description", func(tool *Tool) { tool.Description = "" }, "description: required"},
		{"endpoint not HTTP", func(tool *Tool) { tool.Endpoint = "ftp://x/y" }, "endpoint:"},
		{"bad status endpoint", func(tool *Tool) { tool.StatusEndpoint = "nope" }, "statusEndpoint:"},
		{"missing effect", func(tool *Tool) { tool.Effect = "" }, "effect: must be one of"},
		{"schema not an object", func(tool *Tool) { tool.InputSchema = []byte(`{"type":"string"}`) }, "inputSchema"},
		{"bad secret name", func(tool *Tool) { tool.Secrets = []string{"api-key"} }, "secrets:"},
		{"empty policy condition", func(tool *Tool) { tool.Policy[0].When = "" }, "policy[0].when"},
		{"unknown policy action", func(tool *Tool) { tool.Policy[0].Action = "maybe" }, "policy[0].action"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tool := mustDecode(t, validTool).Tool
			tt.edit(tool)
			assertValidationError(t, tool.Validate(), tt.wantErr)
		})
	}
}

func TestValidationReportsAllProblems(t *testing.T) {
	a := &Agent{Name: "BAD"}
	a.Normalize()
	var ve *ValidationError
	if err := a.Validate(); !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	if len(ve.Problems) < 3 {
		t.Fatalf("got %d problems, want name, provider, model name, and instructions all reported: %v",
			len(ve.Problems), ve.Problems)
	}
}

func TestHashIgnoresFormattingAndOrder(t *testing.T) {
	// Same meaning, different tool order and schema key order.
	a := mustDecode(t, validTool)
	b := mustDecode(t, strings.NewReplacer(
		`{"type": "object", "properties": {"amount": {"type": "number"}}}`,
		`{"properties": {"amount": {"type": "number"}}, "type": "object"}`,
	).Replace(validTool))

	hashA, _ := Hash(a.Tool)
	hashB, _ := Hash(b.Tool)
	if hashA != hashB {
		t.Fatal("equivalent tools have different hashes")
	}

	b.Tool.Description = "Refund an order, carefully"
	if hashC, _ := Hash(b.Tool); hashC == hashA {
		t.Fatal("different tools have the same hash")
	}
}

func TestMaxCostMicroUSD(t *testing.T) {
	// 0.1 + 0.2 is not exactly 0.3 in floating point; rounding makes it exact.
	if got := (Limits{MaxCostUSD: 0.1 + 0.2}).MaxCostMicroUSD(); got != 300_000 {
		t.Fatalf("MaxCostMicroUSD = %d, want 300000", got)
	}
}

func assertValidationError(t *testing.T, err error, want string) {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v, want it to mention %q", err, want)
	}
}
