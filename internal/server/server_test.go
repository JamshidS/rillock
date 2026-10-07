package server

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestRunServesUntilCancelled(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "hello")
	})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, ln, handler, slog.New(slog.DiscardHandler)) }()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+ln.Addr().String()+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "hello" {
		t.Fatalf("body = %q, want %q", body, "hello")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil after a clean shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop after its context was cancelled")
	}
}
