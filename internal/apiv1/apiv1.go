// Package apiv1 defines the JSON bodies of Rillock's HTTP API, version 1.
// The server (package api) and the Go client (package client) share these
// types, so they cannot drift apart.
package apiv1

import (
	"encoding/json"
	"time"
)

// ApplyRequest is the body of POST /v1/apply. Each resource is a JSON object
// with a "kind" field; see package spec.
type ApplyRequest struct {
	Resources []json.RawMessage `json:"resources"`
}

// ApplyResponse lists the outcome for each resource, in request order.
type ApplyResponse struct {
	Results []ApplyResult `json:"results"`
}

// ApplyResult is the outcome for one resource.
type ApplyResult struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Version int    `json:"version"`
	Status  string `json:"status"` // created, updated, or unchanged
}

// SubmitRunRequest is the body of POST /v1/runs.
type SubmitRunRequest struct {
	Agent string          `json:"agent"`
	Input json.RawMessage `json:"input,omitempty"`
}

// Run describes one run.
type Run struct {
	ID              string          `json:"id"`
	Agent           string          `json:"agent"`
	AgentVersion    int             `json:"agentVersion"`
	State           string          `json:"state"`
	CancelRequested bool            `json:"cancelRequested"`
	Input           json.RawMessage `json:"input"`
	Result          json.RawMessage `json:"result,omitempty"`
	FailureReason   string          `json:"failureReason,omitempty"`
	StepsUsed       int             `json:"stepsUsed"`
	InputTokens     int64           `json:"inputTokens"`
	OutputTokens    int64           `json:"outputTokens"`
	CostMicroUSD    int64           `json:"costMicroUSD"`
	CreatedAt       time.Time       `json:"createdAt"`
	UpdatedAt       time.Time       `json:"updatedAt"`
}

// RunList is the body of GET /v1/runs.
type RunList struct {
	Runs []Run `json:"runs"`
}

// Event is one entry in a run's journal.
type Event struct {
	Seq       int             `json:"seq"`
	Type      string          `json:"type"`
	V         int             `json:"v"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"createdAt"`
}

// EventList is the body of GET /v1/runs/{id}/events.
type EventList struct {
	Events []Event `json:"events"`
}

// ErrorResponse is the body of every error response.
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody explains an error. Code is stable and meant for programs; Message
// is meant for people.
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Error codes.
const (
	CodeBadRequest   = "bad_request"  // the request itself is malformed
	CodeInvalidSpec  = "invalid_spec" // a resource failed validation
	CodeUnauthorized = "unauthorized" // missing or unknown API key
	CodeForbidden    = "forbidden"    // the key lacks the required role
	CodeNotFound     = "not_found"    // the run or agent does not exist
	CodeConflict     = "conflict"     // not allowed in the run's current state
	CodeTooLarge     = "too_large"    // request body over the size limit
	CodeInternal     = "internal"     // a server error; details are in the server log
)
