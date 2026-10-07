// Package api serves Rillock's HTTP JSON API.
//
// Handlers are thin: they check the caller's role, decode the request,
// call the store, and encode the response. Rules live elsewhere (spec for
// validation, runstate for the state machine, store for transactions).
package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/jamshids/rillock/internal/auth"
	"github.com/jamshids/rillock/internal/spec"
	"github.com/jamshids/rillock/internal/store"
	"github.com/jamshids/rillock/internal/version"
)

// Store is the part of *store.Store the API uses. Declaring it here, where it
// is used, lets tests substitute a fake without a database.
type Store interface {
	Apply(ctx context.Context, resources []spec.Resource) ([]store.ApplyResult, error)
	SubmitRun(ctx context.Context, agent string, input json.RawMessage) (store.Run, error)
	GetRun(ctx context.Context, id string) (store.Run, error)
	ListRuns(ctx context.Context, f store.ListRunsFilter) ([]store.Run, error)
	CancelRun(ctx context.Context, id, by string) (store.Run, error)
	ListEvents(ctx context.Context, runID string, afterSeq, limit int) ([]store.Event, error)
}

// Authenticator checks API keys. *auth.Keys and *auth.ReloadableKeys satisfy it.
type Authenticator interface {
	Authenticate(key string) (auth.Principal, bool)
}

// API holds what the handlers need.
type API struct {
	store Store
	keys  Authenticator
	log   *slog.Logger
}

// New returns an API backed by st, accepting the keys in keys.
func New(st Store, keys Authenticator, log *slog.Logger) *API {
	return &API{store: st, keys: keys, log: log}
}

// Handler returns the API's routes, wrapped in logging and panic recovery.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()

	// Health checks need no key: load balancers and `curl` must reach them.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeText(w, "ok\n")
	})
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, _ *http.Request) {
		writeText(w, version.Version+"\n")
	})

	mux.Handle("POST /v1/apply", a.require(auth.RoleAdmin, a.apply))
	mux.Handle("POST /v1/runs", a.require(auth.RoleAdmin, a.submitRun))
	mux.Handle("GET /v1/runs", a.require(auth.RoleViewer, a.listRuns))
	mux.Handle("GET /v1/runs/{id}", a.require(auth.RoleViewer, a.getRun))
	mux.Handle("GET /v1/runs/{id}/events", a.require(auth.RoleViewer, a.listEvents))
	mux.Handle("POST /v1/runs/{id}/cancel", a.require(auth.RoleAdmin, a.cancelRun))

	return a.recoverPanics(a.logRequests(mux))
}

func writeText(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(body)) // a failed write means the client left; nothing to do
}
