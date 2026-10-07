// Package model talks to language models.
//
// The types follow the shape most model APIs share (Anthropic's in
// particular): a conversation is a list of messages, and each message holds
// blocks of text, tool calls (tool_use), and tool results (tool_result). The
// agent loop only uses these types, so adding a provider never changes the loop.
package model

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jamshids/rillock/internal/spec"
)

// Provider sends one request to a model and returns its response.
type Provider interface {
	Complete(ctx context.Context, req Request) (Response, error)
}

// Role says who wrote a message.
type Role string

// Roles.
const (
	RoleUser      Role = "user"      // the task, and tool results sent back to the model
	RoleAssistant Role = "assistant" // the model's replies
)

// BlockType is the kind of content in a block.
type BlockType string

// Block types.
const (
	BlockText       BlockType = "text"
	BlockToolUse    BlockType = "tool_use"    // the model asks to call a tool
	BlockToolResult BlockType = "tool_result" // the result of that call, sent back
)

// Block is one piece of a message. Which fields are set depends on Type.
type Block struct {
	Type BlockType `json:"type"`
	Text string    `json:"text,omitempty"` // text, and tool_result content

	// ToolUseID links a tool_use block to the tool_result that answers it.
	ToolUseID string          `json:"toolUseId,omitempty"`
	Name      string          `json:"name,omitempty"`  // tool_use: which tool
	Input     json.RawMessage `json:"input,omitempty"` // tool_use: the arguments
	IsError   bool            `json:"isError,omitempty"`
}

// Message is one turn of the conversation.
type Message struct {
	Role    Role    `json:"role"`
	Content []Block `json:"content"`
}

// Tool describes a tool the model may call.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// Request is one call to a model.
type Request struct {
	Model     string    // provider-specific model name
	System    string    // the agent's instructions
	Messages  []Message // the conversation so far, oldest first
	Tools     []Tool
	MaxTokens int
}

// StopReason says why the model stopped writing.
type StopReason string

// Stop reasons.
const (
	StopEndTurn   StopReason = "end_turn"   // finished: the text is the answer
	StopToolUse   StopReason = "tool_use"   // wants the tool calls in Content run first
	StopMaxTokens StopReason = "max_tokens" // cut off by the length limit
)

// Usage is what one call consumed.
type Usage struct {
	InputTokens  int64 `json:"inputTokens"`
	OutputTokens int64 `json:"outputTokens"`
	CostMicroUSD int64 `json:"costMicroUSD"`
}

// Response is the model's reply.
type Response struct {
	Content    []Block    `json:"content"`
	StopReason StopReason `json:"stopReason"`
	Usage      Usage      `json:"usage"`
}

// ToolCalls returns the tool_use blocks, in the order the model wrote them.
func (r Response) ToolCalls() []Block {
	var calls []Block
	for _, b := range r.Content {
		if b.Type == BlockToolUse {
			calls = append(calls, b)
		}
	}
	return calls
}

// Text returns the text blocks joined together.
func (r Response) Text() string {
	var parts []string
	for _, b := range r.Content {
		if b.Type == BlockText && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// New returns the provider an agent's spec asks for.
func New(m spec.Model) (Provider, error) {
	switch m.Provider {
	case "fake":
		return NewFake(m.Name)
	case "anthropic":
		return nil, fmt.Errorf("model provider %q is not available yet (roadmap Phase E); use provider fake", m.Provider)
	default:
		return nil, fmt.Errorf("unknown model provider %q", m.Provider)
	}
}
