package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jamshids/rillock/internal/apiv1"
	"github.com/jamshids/rillock/internal/auth"
	"github.com/jamshids/rillock/internal/runstate"
	"github.com/jamshids/rillock/internal/spec"
	"github.com/jamshids/rillock/internal/store"
)

// Test keys: one per role.
const (
	adminKey    = "rlk_admin"
	approverKey = "rlk_approver"
	viewerKey   = "rlk_viewer"
)

func testKeys(t *testing.T) *auth.Keys {
	t.Helper()
	var b strings.Builder
	b.WriteString("keys:\n")
	for name, key := range map[string]string{"admin": adminKey, "approver": approverKey, "viewer": viewerKey} {
		fmt.Fprintf(&b, "  - name: %s-key\n    roles: [%s]\n    sha256: %s\n", name, name, auth.Hash(key))
	}
	keys, err := auth.Parse([]byte(b.String()))
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

// fakeStore returns canned results, or err for every call if set.
type fakeStore struct {
	err       error
	cancelled string // name passed to CancelRun
}

const runID = "01928f3e-0000-7000-8000-000000000001"

func (f *fakeStore) Apply(context.Context, []spec.Resource) ([]store.ApplyResult, error) {
	return []store.ApplyResult{{Kind: "Tool", Name: "orders_get", Version: 1, Status: store.ApplyCreated}}, f.err
}
func (f *fakeStore) SubmitRun(_ context.Context, agent string, _ json.RawMessage) (store.Run, error) {
	return store.Run{ID: runID, Agent: agent, State: runstate.StateQueued}, f.err
}
func (f *fakeStore) GetRun(context.Context, string) (store.Run, error) {
	return store.Run{ID: runID, State: runstate.StateQueued}, f.err
}
func (f *fakeStore) ListRuns(context.Context, store.ListRunsFilter) ([]store.Run, error) {
	return nil, f.err
}
func (f *fakeStore) CancelRun(_ context.Context, _, by string) (store.Run, error) {
	f.cancelled = by
	return store.Run{ID: runID, State: runstate.StateCancelled}, f.err
}
func (f *fakeStore) ListEvents(context.Context, string, int, int) ([]store.Event, error) {
	return nil, f.err
}

// response is what a handler wrote.
type response struct {
	StatusCode int
	Header     http.Header
}

// call sends one request to h and returns the response and its body.
func call(t *testing.T, h http.Handler, method, path, key, body string) (response, string) {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return response{StatusCode: rec.Code, Header: rec.Header()}, rec.Body.String()
}

func newTestAPI(t *testing.T, st Store) http.Handler {
	t.Helper()
	return New(st, testKeys(t), slog.New(slog.DiscardHandler)).Handler()
}

func TestAuthentication(t *testing.T) {
	h := newTestAPI(t, &fakeStore{})
	tests := []struct {
		name       string
		method     string
		path       string
		key        string
		body       string
		wantStatus int
	}{
		{"health needs no key", http.MethodGet, "/healthz", "", "", http.StatusOK},
		{"no key", http.MethodGet, "/v1/runs", "", "", http.StatusUnauthorized},
		{"unknown key", http.MethodGet, "/v1/runs", "rlk_nope", "", http.StatusUnauthorized},
		{"viewer may read", http.MethodGet, "/v1/runs", viewerKey, "", http.StatusOK},
		{"approver may read", http.MethodGet, "/v1/runs/" + runID, approverKey, "", http.StatusOK},
		{"viewer may not submit", http.MethodPost, "/v1/runs", viewerKey, `{"agent":"a"}`, http.StatusForbidden},
		{"approver may not apply", http.MethodPost, "/v1/apply", approverKey, `{}`, http.StatusForbidden},
		{"admin may submit", http.MethodPost, "/v1/runs", adminKey, `{"agent":"a"}`, http.StatusCreated},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, body := call(t, h, tt.method, tt.path, tt.key, tt.body)
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body: %s", resp.StatusCode, tt.wantStatus, body)
			}
			if resp.StatusCode == http.StatusUnauthorized && resp.Header.Get("WWW-Authenticate") != "Bearer" {
				t.Error("401 response without a WWW-Authenticate header")
			}
		})
	}
}

func TestCancelRecordsWhoCancelled(t *testing.T) {
	st := &fakeStore{}
	h := newTestAPI(t, st)
	if resp, body := call(t, h, http.MethodPost, "/v1/runs/"+runID+"/cancel", adminKey, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; body: %s", resp.StatusCode, body)
	}
	if st.cancelled != "admin-key" {
		t.Fatalf("CancelRun got by = %q, want the key's name %q", st.cancelled, "admin-key")
	}
}

func TestErrorMapping(t *testing.T) {
	tests := []struct {
		name       string
		storeErr   error
		wantStatus int
		wantCode   string
	}{
		{"not found", fmt.Errorf("run x: %w", store.ErrNotFound), http.StatusNotFound, apiv1.CodeNotFound},
		{"illegal transition", fmt.Errorf("wrapped: %w", runstate.ErrIllegalTransition), http.StatusConflict, apiv1.CodeConflict},
		{"validation", &spec.ValidationError{Kind: "Agent", Name: "a", Problems: []string{"x"}}, http.StatusBadRequest, apiv1.CodeInvalidSpec},
		{"unexpected", errors.New("connection reset by peer at 10.0.0.5"), http.StatusInternalServerError, apiv1.CodeInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTestAPI(t, &fakeStore{err: tt.storeErr})
			resp, body := call(t, h, http.MethodGet, "/v1/runs/"+runID, viewerKey, "")
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			var e apiv1.ErrorResponse
			if err := json.Unmarshal([]byte(body), &e); err != nil || e.Error.Code != tt.wantCode {
				t.Fatalf("body = %s, want error code %q", body, tt.wantCode)
			}
		})
	}
}

func TestInternalErrorsAreNotLeaked(t *testing.T) {
	h := newTestAPI(t, &fakeStore{err: errors.New("password authentication failed for db.internal")})
	_, body := call(t, h, http.MethodGet, "/v1/runs/"+runID, viewerKey, "")
	if strings.Contains(body, "db.internal") {
		t.Fatalf("response leaks internal details: %s", body)
	}
}

func TestBadRequests(t *testing.T) {
	h := newTestAPI(t, &fakeStore{})
	tests := []struct {
		name       string
		method     string
		path       string
		body       string
		wantStatus int
		wantInBody string
	}{
		{"malformed run ID", http.MethodGet, "/v1/runs/not-a-uuid", "", http.StatusBadRequest, "not a run ID"},
		{"unknown state filter", http.MethodGet, "/v1/runs?state=paused", "", http.StatusBadRequest, "unknown state"},
		{"negative limit", http.MethodGet, "/v1/runs?limit=-1", "", http.StatusBadRequest, "non-negative"},
		{"wildcard in ID prefix", http.MethodGet, "/v1/runs?idPrefix=01%25", "", http.StatusBadRequest, "may only contain"},
		{"invalid JSON", http.MethodPost, "/v1/runs", `{"agent":`, http.StatusBadRequest, "invalid JSON"},
		{"unknown field", http.MethodPost, "/v1/runs", `{"agnet":"a"}`, http.StatusBadRequest, "unknown field"},
		{"trailing data", http.MethodPost, "/v1/runs", `{"agent":"a"} {}`, http.StatusBadRequest, "unexpected data"},
		{"missing agent", http.MethodPost, "/v1/runs", `{}`, http.StatusBadRequest, "agent is required"},
		{"nothing to apply", http.MethodPost, "/v1/apply", `{"resources":[]}`, http.StatusBadRequest, "no resources"},
		{"unknown kind", http.MethodPost, "/v1/apply", `{"resources":[{"kind":"Pod"}]}`, http.StatusBadRequest, "unknown kind"},
		{"body too large", http.MethodPost, "/v1/runs", `{"agent":"` + strings.Repeat("a", maxBodyBytes) + `"}`, http.StatusRequestEntityTooLarge, "larger than"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, body := call(t, h, tt.method, tt.path, adminKey, tt.body)
			if resp.StatusCode != tt.wantStatus || !strings.Contains(body, tt.wantInBody) {
				t.Fatalf("got %d %s, want %d containing %q", resp.StatusCode, body, tt.wantStatus, tt.wantInBody)
			}
		})
	}
}

func TestApplyReportsEveryInvalidResource(t *testing.T) {
	h := newTestAPI(t, &fakeStore{})
	body := `{"resources":[
		{"kind":"Tool","name":"Bad Name","description":"d","endpoint":"http://x","effect":"keyed"},
		{"kind":"Agent","name":"a","model":{"provider":"nope","name":"m"},"instructions":"i"}
	]}`
	resp, got := call(t, h, http.MethodPost, "/v1/apply", adminKey, body)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(got, apiv1.CodeInvalidSpec) {
		t.Fatalf("got %d %s, want 400 invalid_spec", resp.StatusCode, got)
	}
	if !strings.Contains(got, "Tool Bad Name") || !strings.Contains(got, "Agent a") {
		t.Fatalf("response does not mention both invalid resources: %s", got)
	}
}

func TestApplyRejectsDuplicateResources(t *testing.T) {
	h := newTestAPI(t, &fakeStore{})
	tool := `{"kind":"Tool","name":"orders_get","description":"d","endpoint":"http://x","effect":"read_only"}`
	resp, body := call(t, h, http.MethodPost, "/v1/apply", adminKey, `{"resources":[`+tool+`,`+tool+`]}`)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "appears twice") {
		t.Fatalf("got %d %s, want 400 mentioning the duplicate", resp.StatusCode, body)
	}
}

type panicStore struct{ fakeStore }

func (panicStore) GetRun(context.Context, string) (store.Run, error) { panic("boom") }

func TestPanicBecomes500(t *testing.T) {
	h := newTestAPI(t, &panicStore{})
	resp, body := call(t, h, http.MethodGet, "/v1/runs/"+runID, viewerKey, "")
	if resp.StatusCode != http.StatusInternalServerError || strings.Contains(body, "boom") {
		t.Fatalf("got %d %s, want a 500 that hides the panic message", resp.StatusCode, body)
	}
}
