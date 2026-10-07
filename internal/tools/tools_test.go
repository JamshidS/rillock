package tools

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jamshids/rillock/internal/spec"
)

func call(endpoint string) Call {
	return Call{
		Tool:           spec.Tool{Name: "orders_get", Endpoint: endpoint},
		Input:          json.RawMessage(`{"order_id":"ord_812"}`),
		IdempotencyKey: "run-1:3",
		RunID:          "run-1",
		Step:           3,
	}
}

func TestExecuteSendsCallAndReadsJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case r.Method != http.MethodPost:
			t.Errorf("method = %s", r.Method)
		case string(body) != `{"order_id":"ord_812"}`:
			t.Errorf("body = %s", body)
		case r.Header.Get("Idempotency-Key") != "run-1:3" || r.Header.Get("Rillock-Step") != "3":
			t.Errorf("headers = %v", r.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `  {"amount": 300}  `)
	}))
	defer srv.Close()

	res, err := NewHTTPExecutor(DefaultTimeout).Execute(t.Context(), call(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || res.Status != http.StatusOK || string(res.Body) != `{"amount": 300}` {
		t.Fatalf("result = %+v (body %s)", res, res.Body)
	}
}

func TestToolErrorsAreResults(t *testing.T) {
	// A tool that answers with an error still answered: the model gets to see
	// the error and may try something else.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "order not found", http.StatusNotFound)
	}))
	defer srv.Close()

	res, err := NewHTTPExecutor(DefaultTimeout).Execute(t.Context(), call(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || res.Status != http.StatusNotFound || string(res.Body) != `"order not found\n"` {
		t.Fatalf("result = %+v (body %s)", res, res.Body)
	}
}

func TestUnreachable(t *testing.T) {
	_, err := NewHTTPExecutor(DefaultTimeout).Execute(t.Context(), call("http://127.0.0.1:1/"))
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable", err)
	}
}

func TestTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release // hang until the test is over
	}))
	defer srv.Close()
	defer close(release) // deferred calls run last-in first-out: this runs before srv.Close

	start := time.Now()
	_, err := NewHTTPExecutor(100*time.Millisecond).Execute(t.Context(), call(srv.URL))
	if !errors.Is(err, ErrUnreachable) || time.Since(start) > 2*time.Second {
		t.Fatalf("err = %v after %v, want ErrUnreachable quickly", err, time.Since(start))
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	var redirected bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected = true }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	res, err := NewHTTPExecutor(DefaultTimeout).Execute(t.Context(), call(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if redirected || res.Status != http.StatusTemporaryRedirect || !res.IsError {
		t.Fatalf("redirect followed = %v, result = %+v", redirected, res)
	}
}

func TestOversizedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("x", MaxResponseBytes+10))
	}))
	defer srv.Close()

	res, err := NewHTTPExecutor(DefaultTimeout).Execute(t.Context(), call(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(string(res.Body), "larger than") {
		t.Fatalf("result = %+v", res)
	}
}
