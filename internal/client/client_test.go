package client

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jamshids/rillock/internal/apiv1"
)

func TestSendsKeyAndDecodesResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer rlk_test" {
			t.Errorf("Authorization = %q", got)
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/runs" {
			t.Errorf("request = %s %s, want POST /v1/runs", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"agent":"refund"`) {
			t.Errorf("body = %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"r1","agent":"refund","state":"queued"}`)
	}))
	defer srv.Close()

	c, err := New(srv.URL, "rlk_test")
	if err != nil {
		t.Fatal(err)
	}
	run, err := c.SubmitRun(t.Context(), "refund", nil)
	if err != nil {
		t.Fatalf("SubmitRun: %v", err)
	}
	if run.ID != "r1" || run.State != "queued" {
		t.Fatalf("run = %+v", run)
	}
}

func TestErrorResponses(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		wantCode string
		wantMsg  string
	}{
		{"API error", http.StatusNotFound, `{"error":{"code":"not_found","message":"run x: not found"}}`,
			apiv1.CodeNotFound, "run x: not found"},
		{"JSON without our error body", http.StatusBadGateway, `{}`, "", "server returned 502 Bad Gateway"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer srv.Close()

			c, _ := New(srv.URL, "k")
			_, err := c.GetRun(t.Context(), "x")
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("err = %v, want *APIError", err)
			}
			if apiErr.Status != tt.status || apiErr.Code != tt.wantCode || apiErr.Message != tt.wantMsg {
				t.Fatalf("got %+v", apiErr)
			}
		})
	}
}

// TestWrongServer reproduces RILLOCK_SERVER pointing at another program: here
// one that redirects to an HTML page, like cadvisor on port 8080.
func TestWrongServer(t *testing.T) {
	var sawKey bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/containers/" {
			sawKey = r.Header.Get("Authorization") != ""
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, "<html>dashboard</html>")
			return
		}
		http.Redirect(w, r, "/containers/", http.StatusTemporaryRedirect)
	}))
	defer other.Close()

	c, _ := New(other.URL, "rlk_secret")
	_, err := c.SubmitRun(t.Context(), "refund", nil)
	if err == nil || !strings.Contains(err.Error(), "did not answer like a Rillock server") ||
		!strings.Contains(err.Error(), "RILLOCK_SERVER") {
		t.Fatalf("err = %v, want a clear wrong-server message", err)
	}
	if sawKey {
		t.Fatal("the API key was sent to the redirect target")
	}
}

func TestNotJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html>hello</html>")
	}))
	defer srv.Close()

	c, _ := New(srv.URL, "k")
	_, err := c.ListRuns(t.Context(), ListRunsOptions{})
	if err == nil || !strings.Contains(err.Error(), `Content-Type "text/html"`) {
		t.Fatalf("err = %v, want it to name the unexpected content type", err)
	}
}

func TestServerNotRunning(t *testing.T) {
	c, _ := New("http://127.0.0.1:1", "k")
	_, err := c.ListRuns(t.Context(), ListRunsOptions{})
	if err == nil || !strings.Contains(err.Error(), "is `rillock server` running?") {
		t.Fatalf("err = %v, want a hint that the server is not running", err)
	}
}

func TestNewRejectsBadURL(t *testing.T) {
	for _, u := range []string{"", "localhost:8080", "ftp://x"} {
		if _, err := New(u, "k"); err == nil {
			t.Errorf("New(%q) succeeded", u)
		}
	}
}
