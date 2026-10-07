// Package client calls Rillock's HTTP API. The CLI uses it; so can any Go program.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/jamshids/rillock/internal/apiv1"
)

// Client talks to one Rillock server with one API key.
type Client struct {
	base *url.URL
	key  string
	http *http.Client
}

// New returns a client for the server at serverURL, such as "http://127.0.0.1:8080".
func New(serverURL, apiKey string) (*Client, error) {
	u, err := url.Parse(serverURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("invalid server URL %q", serverURL)
	}
	return &Client{
		base: u,
		key:  apiKey,
		http: &http.Client{
			// Without a timeout, a server that stops answering would hang the CLI forever.
			Timeout: 30 * time.Second,
			// A Rillock server never redirects. A redirect means RILLOCK_SERVER
			// points at something else, and following it would send the API
			// key to wherever the redirect leads.
			CheckRedirect: func(req *http.Request, _ []*http.Request) error {
				return fmt.Errorf("%w: it redirected to %s", errNotRillock, req.URL.Redacted())
			},
		},
	}, nil
}

// errNotRillock means whatever answered at the server URL is not a Rillock server.
var errNotRillock = errors.New("the server did not answer like a Rillock server")

// APIError is an error response from the server.
type APIError struct {
	Status  int    // HTTP status code
	Code    string // stable code, see the apiv1 Code constants
	Message string
}

func (e *APIError) Error() string {
	return e.Message
}

// Apply creates or updates resources. Each resource is a JSON object with a "kind".
func (c *Client) Apply(ctx context.Context, resources []json.RawMessage) (apiv1.ApplyResponse, error) {
	var resp apiv1.ApplyResponse
	err := c.do(ctx, http.MethodPost, "/v1/apply", nil, apiv1.ApplyRequest{Resources: resources}, &resp)
	return resp, err
}

// SubmitRun queues a run of the named agent.
func (c *Client) SubmitRun(ctx context.Context, agent string, input json.RawMessage) (apiv1.Run, error) {
	var run apiv1.Run
	err := c.do(ctx, http.MethodPost, "/v1/runs", nil, apiv1.SubmitRunRequest{Agent: agent, Input: input}, &run)
	return run, err
}

// GetRun returns one run.
func (c *Client) GetRun(ctx context.Context, id string) (apiv1.Run, error) {
	var run apiv1.Run
	err := c.do(ctx, http.MethodGet, "/v1/runs/"+url.PathEscape(id), nil, nil, &run)
	return run, err
}

// ListRunsOptions narrows ListRuns. The zero value lists the newest runs.
type ListRunsOptions struct {
	State    string // only runs in this state
	IDPrefix string // only runs whose ID starts with this
	Limit    int    // at most this many; 0 means the server's default
}

// ListRuns returns runs, newest first.
func (c *Client) ListRuns(ctx context.Context, opts ListRunsOptions) ([]apiv1.Run, error) {
	q := url.Values{}
	if opts.State != "" {
		q.Set("state", opts.State)
	}
	if opts.IDPrefix != "" {
		q.Set("idPrefix", opts.IDPrefix)
	}
	if opts.Limit > 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	var list apiv1.RunList
	err := c.do(ctx, http.MethodGet, "/v1/runs", q, nil, &list)
	return list.Runs, err
}

// CancelRun stops a run, or asks its worker to stop it if it is running.
func (c *Client) CancelRun(ctx context.Context, id string) (apiv1.Run, error) {
	var run apiv1.Run
	err := c.do(ctx, http.MethodPost, "/v1/runs/"+url.PathEscape(id)+"/cancel", nil, nil, &run)
	return run, err
}

// ListEvents returns a run's events after sequence number afterSeq.
func (c *Client) ListEvents(ctx context.Context, id string, afterSeq int) ([]apiv1.Event, error) {
	q := url.Values{}
	if afterSeq > 0 {
		q.Set("after", strconv.Itoa(afterSeq))
	}
	var list apiv1.EventList
	err := c.do(ctx, http.MethodGet, "/v1/runs/"+url.PathEscape(id)+"/events", q, nil, &list)
	return list.Events, err
}

// do sends one request. in (if not nil) is encoded as the JSON body; a
// successful response is decoded into out.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, in, out any) error {
	u := c.base.JoinPath(path)
	u.RawQuery = query.Encode()

	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if errors.Is(err, errNotRillock) {
		return fmt.Errorf("%w (is RILLOCK_SERVER=%s the right address?)", err, c.base)
	}
	if err != nil {
		var opErr *net.OpError
		if errors.As(err, &opErr) && opErr.Op == "dial" {
			return fmt.Errorf("cannot reach the rillock server at %s (is `rillock server` running?): %w", c.base, err)
		}
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Every Rillock response, including errors, is JSON. Anything else (an
	// HTML page, a proxy's error) comes from some other program.
	if mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mediaType != "application/json" {
		return fmt.Errorf("%w: got %s with Content-Type %q (is RILLOCK_SERVER=%s the right address?)",
			errNotRillock, resp.Status, resp.Header.Get("Content-Type"), c.base)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return decodeError(resp)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode %s %s response: %w", method, path, err)
	}
	return nil
}

func decodeError(resp *http.Response) error {
	var e apiv1.ErrorResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&e); err != nil || e.Error.Message == "" {
		return &APIError{Status: resp.StatusCode, Code: "", Message: "server returned " + resp.Status}
	}
	return &APIError{Status: resp.StatusCode, Code: e.Error.Code, Message: e.Error.Message}
}
