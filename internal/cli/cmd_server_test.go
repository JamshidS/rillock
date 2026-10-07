package cli

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jamshids/rillock/internal/auth"
)

// syncBuffer is a bytes.Buffer safe to write from one goroutine and read from
// another, as the log is here.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestReloadOnSignal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.yaml")
	writeEntry := func(entries ...auth.Entry) {
		t.Helper()
		_ = os.Remove(path)
		for _, e := range entries {
			if err := auth.AddToFile(path, e); err != nil {
				t.Fatal(err)
			}
		}
	}
	writeEntry(auth.Entry{Name: "a", Roles: []auth.Role{auth.RoleAdmin}, SHA256: auth.Hash("old")})
	keys, err := auth.LoadReloadable(path)
	if err != nil {
		t.Fatal(err)
	}

	var logs syncBuffer
	sig := make(chan os.Signal)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		reloadOnSignal(ctx, sig, keys, slog.New(slog.NewTextHandler(&logs, nil)))
		close(done)
	}()

	// Add a key, then "send SIGHUP". The channel is unbuffered, so the send
	// returns only once reloadOnSignal has received it.
	writeEntry(
		auth.Entry{Name: "a", Roles: []auth.Role{auth.RoleAdmin}, SHA256: auth.Hash("old")},
		auth.Entry{Name: "b", Roles: []auth.Role{auth.RoleViewer}, SHA256: auth.Hash("new")},
	)
	sig <- syscall.SIGHUP
	waitFor(t, func() bool { _, ok := keys.Authenticate("new"); return ok }, "new key after reload")

	// A broken file is reported and the keys keep working.
	if err := os.WriteFile(path, []byte("keys: [unclosed"), 0o600); err != nil {
		t.Fatal(err)
	}
	sig <- syscall.SIGHUP
	waitFor(t, func() bool { return strings.Contains(logs.String(), "reloading keys failed") }, "failure logged")
	if _, ok := keys.Authenticate("new"); !ok {
		t.Fatal("a failed reload removed the working keys")
	}

	// Cancelling the context stops the goroutine.
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reloadOnSignal did not stop after its context was cancelled")
	}
}

// waitFor polls cond until it is true or a deadline passes.
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
