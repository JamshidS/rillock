// Package tools calls the HTTP endpoints behind an agent's tools.
//
// A call is a POST of the arguments as JSON. Every request carries the step's
// idempotency key, so a tool can recognise a retried call (Phase F builds
// exactly-once execution on this).
//
// Tool output is untrusted (invariant 7): it is returned as data for the
// model to read, and nothing in Rillock ever acts on its contents.
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/jamshids/rillock/internal/spec"
)

// Call is one tool invocation.
type Call struct {
	Tool           spec.Tool
	Input          json.RawMessage
	IdempotencyKey string
	RunID          string
	Step           int
}

// Result is what the tool answered. The call happened; IsError says whether
// it failed. Only a 2xx status is success: an error status, and also a
// redirect (which is not followed), mean the tool did not do its job.
type Result struct {
	Status  int             `json:"status"`
	Body    json.RawMessage `json:"body"` // the tool's JSON, or its text as a JSON string
	IsError bool            `json:"isError"`
}

// ErrUnreachable means the tool could not be called at all: the endpoint
// refused the connection, timed out, or sent no usable answer. The run cannot
// continue, which is different from a tool that answered with an error.
var ErrUnreachable = errors.New("tool unreachable")

// Limits on a single call.
const (
	DefaultTimeout = 30 * time.Second
	// MaxResponseBytes bounds what a tool may send back. The result goes into
	// the model's context, where a huge answer would cost money and time.
	MaxResponseBytes = 256 << 10 // 256 KiB
)

// HTTPExecutor calls tools over HTTP.
type HTTPExecutor struct {
	client *http.Client
}

// NewHTTPExecutor returns an executor whose calls time out after timeout.
func NewHTTPExecutor(timeout time.Duration) *HTTPExecutor {
	return &HTTPExecutor{client: &http.Client{
		Timeout: timeout,
		// A tool's endpoint is fixed in its spec. Following a redirect would
		// send the call somewhere the spec never approved.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// Execute calls the tool. It returns an error wrapping ErrUnreachable when no
// answer was received; any answer, including an HTTP error, is a Result.
func (e *HTTPExecutor) Execute(ctx context.Context, c Call) (Result, error) {
	input := c.Input
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Tool.Endpoint, bytes.NewReader(input))
	if err != nil {
		return Result{}, fmt.Errorf("%w: %s: %w", ErrUnreachable, c.Tool.Name, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", c.IdempotencyKey)
	req.Header.Set("Rillock-Run-Id", c.RunID)
	req.Header.Set("Rillock-Step", strconv.Itoa(c.Step))

	resp, err := e.client.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %s: %w", ErrUnreachable, c.Tool.Name, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Read one byte more than allowed, to tell "exactly the limit" from "over it".
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		return Result{}, fmt.Errorf("%w: %s: reading the response: %w", ErrUnreachable, c.Tool.Name, err)
	}
	if len(body) > MaxResponseBytes {
		return Result{
			Status:  resp.StatusCode,
			Body:    jsonString(fmt.Sprintf("the tool's response is larger than %d bytes and was discarded", MaxResponseBytes)),
			IsError: true,
		}, nil
	}

	return Result{
		Status:  resp.StatusCode,
		Body:    asJSON(body),
		IsError: resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices, // not 2xx
	}, nil
}

// asJSON keeps a JSON answer as it is and turns anything else into a JSON
// string, so a result can always be stored and shown the same way.
func asJSON(body []byte) json.RawMessage {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && json.Valid(trimmed) {
		return trimmed
	}
	return jsonString(string(body))
}

func jsonString(s string) json.RawMessage {
	b, _ := json.Marshal(s) // marshalling a string cannot fail
	return b
}
