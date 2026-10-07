package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"

	"github.com/jamshids/rillock/internal/apiv1"
	"github.com/jamshids/rillock/internal/runstate"
	"github.com/jamshids/rillock/internal/spec"
	"github.com/jamshids/rillock/internal/store"
)

// maxResourcesPerApply bounds one apply request.
const maxResourcesPerApply = 100

func (a *API) apply(w http.ResponseWriter, r *http.Request) {
	var req apiv1.ApplyRequest
	if err := decodeJSON(w, r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	resources, err := decodeResources(req.Resources)
	if err != nil {
		a.fail(w, r, err)
		return
	}

	results, err := a.store.Apply(r.Context(), resources)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	resp := apiv1.ApplyResponse{Results: make([]apiv1.ApplyResult, len(results))}
	for i, res := range results {
		resp.Results[i] = apiv1.ApplyResult{Kind: res.Kind, Name: res.Name, Version: res.Version, Status: string(res.Status)}
	}
	writeJSON(w, http.StatusOK, resp)
}

// decodeResources decodes and validates every resource, reporting all invalid
// resources at once rather than stopping at the first.
func decodeResources(raw []json.RawMessage) ([]spec.Resource, error) {
	switch {
	case len(raw) == 0:
		return nil, fmt.Errorf("%w: no resources to apply", errBadRequest)
	case len(raw) > maxResourcesPerApply:
		return nil, fmt.Errorf("%w: at most %d resources per request", errBadRequest, maxResourcesPerApply)
	}

	resources := make([]spec.Resource, 0, len(raw))
	seen := map[string]bool{}
	var errs []error
	for i, data := range raw {
		res, err := spec.Decode(data)
		if err != nil {
			errs = append(errs, fmt.Errorf("resource %d: %w", i+1, err))
			continue
		}
		var verr error
		if res.Agent != nil {
			verr = res.Agent.Validate()
		} else {
			verr = res.Tool.Validate()
		}
		if verr != nil {
			errs = append(errs, verr)
			continue
		}
		id := res.Kind() + "/" + res.Name()
		if seen[id] {
			errs = append(errs, fmt.Errorf("%s appears twice in one request", id))
			continue
		}
		seen[id] = true
		resources = append(resources, res)
	}

	if len(errs) == 0 {
		return resources, nil
	}
	// If every problem is a validation error, the caller gets the specific
	// invalid_spec code; any decode error makes the whole request malformed.
	kind := errInvalidSpec
	for _, err := range errs {
		var ve *spec.ValidationError
		if !errors.As(err, &ve) {
			kind = errBadRequest
			break
		}
	}
	return nil, fmt.Errorf("%w:\n%w", kind, errors.Join(errs...))
}

func (a *API) submitRun(w http.ResponseWriter, r *http.Request) {
	var req apiv1.SubmitRunRequest
	if err := decodeJSON(w, r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	if req.Agent == "" {
		a.fail(w, r, fmt.Errorf("%w: agent is required", errBadRequest))
		return
	}
	run, err := a.store.SubmitRun(r.Context(), req.Agent, req.Input)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toRun(run))
}

func (a *API) listRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := store.ListRunsFilter{State: runstate.State(q.Get("state"))}
	if filter.State != "" && !slices.Contains(runstate.States(), filter.State) {
		a.fail(w, r, fmt.Errorf("%w: unknown state %q", errBadRequest, filter.State))
		return
	}
	filter.IDPrefix = q.Get("idPrefix")
	if filter.IDPrefix != "" && !idPrefixPattern.MatchString(filter.IDPrefix) {
		a.fail(w, r, fmt.Errorf("%w: idPrefix %q may only contain 0-9, a-f, and '-'", errBadRequest, filter.IDPrefix))
		return
	}
	limit, err := intParam(q.Get("limit"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	filter.Limit = limit

	runs, err := a.store.ListRuns(r.Context(), filter)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	resp := apiv1.RunList{Runs: make([]apiv1.Run, len(runs))}
	for i, run := range runs {
		resp.Runs[i] = toRun(run)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *API) getRun(w http.ResponseWriter, r *http.Request) {
	id, ok := a.runID(w, r)
	if !ok {
		return
	}
	run, err := a.store.GetRun(r.Context(), id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toRun(run))
}

func (a *API) listEvents(w http.ResponseWriter, r *http.Request) {
	id, ok := a.runID(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	after, err := intParam(q.Get("after"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	limit, err := intParam(q.Get("limit"))
	if err != nil {
		a.fail(w, r, err)
		return
	}

	events, err := a.store.ListEvents(r.Context(), id, after, limit)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	resp := apiv1.EventList{Events: make([]apiv1.Event, len(events))}
	for i, e := range events {
		resp.Events[i] = apiv1.Event{Seq: e.Seq, Type: e.Type, V: e.V, Payload: e.Payload, CreatedAt: e.CreatedAt}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *API) cancelRun(w http.ResponseWriter, r *http.Request) {
	id, ok := a.runID(w, r)
	if !ok {
		return
	}
	run, err := a.store.CancelRun(r.Context(), id, principalFrom(r.Context()).Name)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toRun(run))
}

var (
	uuidPattern     = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	idPrefixPattern = regexp.MustCompile(`^[0-9a-f-]{1,36}$`)
)

// runID reads the {id} path value. A malformed ID is rejected here with 400,
// before it reaches the database as an invalid UUID.
func (a *API) runID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !uuidPattern.MatchString(id) {
		a.fail(w, r, fmt.Errorf("%w: %q is not a run ID", errBadRequest, id))
		return "", false
	}
	return id, true
}

// intParam parses an optional non-negative integer query parameter.
func intParam(s string) (int, error) {
	if s == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%w: %q is not a non-negative integer", errBadRequest, s)
	}
	return n, nil
}

func toRun(r store.Run) apiv1.Run {
	return apiv1.Run{
		ID:              r.ID,
		Agent:           r.Agent,
		AgentVersion:    r.AgentVersion,
		State:           string(r.State),
		CancelRequested: r.CancelRequested,
		Input:           r.Input,
		Result:          r.Result,
		FailureReason:   r.FailureReason,
		StepsUsed:       r.StepsUsed,
		InputTokens:     r.InputTokens,
		OutputTokens:    r.OutputTokens,
		CostMicroUSD:    r.CostMicroUSD,
		CreatedAt:       r.CreatedAt,
		UpdatedAt:       r.UpdatedAt,
	}
}
