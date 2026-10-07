package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/jamshids/rillock/internal/apiv1"
	"github.com/jamshids/rillock/internal/auth"
)

// principalKey is the context key for the authenticated caller. An unexported
// type means no other package can read or overwrite the value by accident.
type principalKey struct{}

// principalFrom returns the caller that require authenticated.
func principalFrom(ctx context.Context) auth.Principal {
	p, _ := ctx.Value(principalKey{}).(auth.Principal)
	return p
}

// require wraps next so it only runs for callers whose key grants role.
// A missing or unknown key gets 401; a known key without the role gets 403.
func (a *API) require(role auth.Role, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, ok := bearerToken(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, apiv1.CodeUnauthorized,
				"missing API key: send the header Authorization: Bearer <key>")
			return
		}
		p, ok := a.keys.Authenticate(key)
		if !ok {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, apiv1.CodeUnauthorized,
				"unknown API key (after adding a key, send the server SIGHUP to reload its keys: make reload-keys)")
			return
		}
		if !p.Has(role) {
			writeError(w, http.StatusForbidden, apiv1.CodeForbidden,
				"key "+p.Name+" does not have the "+string(role)+" role")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	})
}

func bearerToken(r *http.Request) (string, bool) {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return "", false
	}
	return token, true
}

// statusRecorder remembers the status code a handler wrote, for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// logRequests logs one line per request. It never logs headers or bodies,
// which may contain API keys or customer data.
func (a *API) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		a.log.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration", time.Since(start).Round(time.Microsecond))
	})
}

// recoverPanics turns a panic in a handler into a 500 response and a log line,
// instead of a dropped connection with no explanation.
func (a *API) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler { //nolint:errorlint // a sentinel panic value, not a wrapped error
					panic(v) // the standard library uses this to abort a response on purpose
				}
				a.log.Error("handler panicked", "method", r.Method, "path", r.URL.Path, "panic", v)
				writeError(w, http.StatusInternalServerError, apiv1.CodeInternal, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
