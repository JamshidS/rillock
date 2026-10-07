package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/jamshids/rillock/internal/apiv1"
	"github.com/jamshids/rillock/internal/runstate"
	"github.com/jamshids/rillock/internal/spec"
	"github.com/jamshids/rillock/internal/store"
)

// maxBodyBytes limits request bodies, so one request cannot exhaust memory.
const maxBodyBytes = 1 << 20 // 1 MiB

// Errors caused by the request (400). Handlers wrap them with details.
var (
	errBadRequest  = errors.New("bad request")
	errInvalidSpec = errors.New("invalid resources")
)

// decodeJSON reads the request body into dst. It rejects unknown fields,
// trailing data, and bodies over maxBodyBytes.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return err
		}
		return fmt.Errorf("%w: invalid JSON body: %w", errBadRequest, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: unexpected data after the JSON body", errBadRequest)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	// Responses are never embedded in HTML, so keep "<key>" readable instead
	// of escaping it as "\u003ckey\u003e".
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v) // a failed write means the client left; nothing to do
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, apiv1.ErrorResponse{Error: apiv1.ErrorBody{Code: code, Message: message}})
}

// fail maps an error to an HTTP response. This is the one place that decides
// which errors are the caller's fault (4xx) and which are ours (500).
func (a *API) fail(w http.ResponseWriter, r *http.Request, err error) {
	var invalid *spec.ValidationError
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &invalid), errors.Is(err, errInvalidSpec):
		writeError(w, http.StatusBadRequest, apiv1.CodeInvalidSpec, err.Error())
	case errors.Is(err, errBadRequest):
		writeError(w, http.StatusBadRequest, apiv1.CodeBadRequest, err.Error())
	case errors.As(err, &tooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, apiv1.CodeTooLarge,
			fmt.Sprintf("request body is larger than %d bytes", tooLarge.Limit))
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, apiv1.CodeNotFound, err.Error())
	case errors.Is(err, runstate.ErrIllegalTransition):
		writeError(w, http.StatusConflict, apiv1.CodeConflict, err.Error())
	default:
		// Our fault. Log the details; tell the caller only that it failed, since
		// internal errors can mention table names, queries, or hostnames.
		a.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		writeError(w, http.StatusInternalServerError, apiv1.CodeInternal, "internal error")
	}
}
