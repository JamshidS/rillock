package cli

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jamshids/rillock/internal/api"
	"github.com/jamshids/rillock/internal/auth"
	"github.com/jamshids/rillock/internal/config"
	"github.com/jamshids/rillock/internal/store"
	"github.com/jamshids/rillock/internal/testdb"
	rtools "github.com/jamshids/rillock/internal/tools"
	"github.com/jamshids/rillock/internal/worker"
)

// result is what one command printed and returned.
type result struct {
	code   int
	stdout string
	stderr string
}

// rillock runs one command with the given environment.
func rillock(t *testing.T, env map[string]string, args ...string) result {
	t.Helper()
	var stdout, stderr bytes.Buffer
	getenv := config.Env(func(k string) string { return env[k] })
	code := Run(t.Context(), args, getenv, &stdout, &stderr)
	return result{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

// testServer starts the real API on a fresh database and returns the
// environment a CLI user would have: server URL and an admin key.
func testServer(t *testing.T) map[string]string {
	t.Helper()
	env, _ := testServerWithStore(t)
	return env
}

// testServerWithStore is testServer that also returns the store, for tests
// that run a worker next to the API.
func testServerWithStore(t *testing.T) (map[string]string, *store.Store) {
	t.Helper()
	ctx := t.Context()
	url := testdb.New(t)
	if _, err := store.Migrate(ctx, url); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)

	keys, err := auth.Parse(fmt.Appendf(nil,
		"keys:\n  - name: tester\n    roles: [admin]\n    sha256: %s\n  - name: watcher\n    roles: [viewer]\n    sha256: %s\n",
		auth.Hash("rlk_admin"), auth.Hash("rlk_viewer")))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(api.New(st, keys, slog.New(slog.DiscardHandler)).Handler())
	t.Cleanup(srv.Close)
	return map[string]string{"RILLOCK_SERVER": srv.URL, "RILLOCK_API_KEY": "rlk_admin"}, st
}

var runIDPattern = regexp.MustCompile(`run/([0-9a-f-]{36}) submitted`)

func TestApplyRunGetCancel(t *testing.T) {
	env := testServer(t)

	r := rillock(t, env, "apply", "-f", "testdata/refund.yaml")
	wantApply := "tool/orders_get created (version 1)\n" +
		"tool/payments_refund created (version 1)\n" +
		"agent/refund-agent created (version 1)\n"
	if r.code != 0 || r.stdout != wantApply {
		t.Fatalf("apply: code %d\nstdout:\n%s\nstderr:\n%s", r.code, r.stdout, r.stderr)
	}

	if r := rillock(t, env, "apply", "-f", "testdata/refund.yaml"); !strings.Contains(r.stdout, "agent/refund-agent unchanged (version 1)") {
		t.Fatalf("second apply should change nothing:\n%s%s", r.stdout, r.stderr)
	}

	// Flags after the agent name must work.
	r = rillock(t, env, "run", "refund-agent", "--input", "Order 812 arrived broken")
	m := runIDPattern.FindStringSubmatch(r.stdout)
	if r.code != 0 || m == nil {
		t.Fatalf("run: code %d\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
	}
	id := m[1]

	r = rillock(t, env, "get", "runs")
	if r.code != 0 || !strings.Contains(r.stdout, id) || !strings.Contains(r.stdout, "queued") {
		t.Fatalf("get runs:\n%s%s", r.stdout, r.stderr)
	}

	r = rillock(t, env, "get", "run", id)
	for _, want := range []string{"refund-agent (version 1)", "queued", `"Order 812 arrived broken"`, "RunCreated"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("get run output lacks %q:\n%s", want, r.stdout)
		}
	}

	if r := rillock(t, env, "cancel", id); r.code != 0 || !strings.Contains(r.stdout, "cancelled") {
		t.Fatalf("cancel: code %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	r = rillock(t, env, "cancel", id)
	if r.code != exitError || !strings.Contains(r.stderr, "illegal run state transition") {
		t.Fatalf("cancelling twice: code %d, stderr %q; want an illegal-transition error", r.code, r.stderr)
	}

	if r := rillock(t, env, "get", "runs", "--state", "cancelled"); !strings.Contains(r.stdout, id) {
		t.Fatalf("get runs --state cancelled does not list the run:\n%s", r.stdout)
	}
}

func TestViewerCannotApply(t *testing.T) {
	env := testServer(t)
	env["RILLOCK_API_KEY"] = "rlk_viewer"
	r := rillock(t, env, "apply", "-f", "testdata/refund.yaml")
	if r.code != exitError || !strings.Contains(r.stderr, "does not have the admin role") {
		t.Fatalf("code %d, stderr %q", r.code, r.stderr)
	}
}

func TestApplyShowsValidationProblems(t *testing.T) {
	env := testServer(t)
	path := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(path, []byte("kind: Tool\nname: payments.refund\nefect: keyed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := rillock(t, env, "apply", "-f", path)
	if r.code != exitError || !strings.Contains(r.stderr, `unknown field "efect"`) {
		t.Fatalf("code %d, stderr %q", r.code, r.stderr)
	}
}

func TestRunUnknownAgent(t *testing.T) {
	env := testServer(t)
	r := rillock(t, env, "run", "nobody")
	if r.code != exitError || !strings.Contains(r.stderr, `agent "nobody": not found`) {
		t.Fatalf("code %d, stderr %q", r.code, r.stderr)
	}
}

// The tests below need no database.

func TestUsageErrors(t *testing.T) {
	env := map[string]string{"RILLOCK_API_KEY": "k"}
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"no command", nil, "Commands:"},
		{"unknown command", []string{"deploy"}, `unknown command "deploy"`},
		{"run without agent", []string{"run"}, "expected exactly one agent name"},
		{"both inputs", []string{"run", "a", "--input", "x", "--input-json", "1"}, "not both"},
		{"invalid JSON input", []string{"run", "a", "--input-json", "{"}, "not valid JSON"},
		{"apply without files", []string{"apply"}, "at least one -f"},
		{"get what", []string{"get", "agents"}, "expected `get runs` or `get run <id>`"},
		{"unknown flag", []string{"get", "runs", "--colour"}, "flag provided but not defined"},
		{"keys without name", []string{"keys", "generate", "--roles", "admin"}, "--name and --roles are required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := rillock(t, env, tt.args...)
			if r.code != exitUsage || !strings.Contains(r.stderr, tt.wantErr) {
				t.Fatalf("code %d, stderr:\n%s\nwant exit 2 mentioning %q", r.code, r.stderr, tt.wantErr)
			}
		})
	}
}

func TestHelp(t *testing.T) {
	if r := rillock(t, nil, "help"); r.code != 0 || !strings.Contains(r.stdout, "apply") {
		t.Fatalf("help: code %d\n%s", r.code, r.stdout)
	}
	r := rillock(t, nil, "migrate", "-h")
	if r.code != 0 || !strings.Contains(r.stderr, "-database-url") || strings.Contains(r.stderr, "-addr") {
		t.Fatalf("migrate -h should list only its own flags:\n%s", r.stderr)
	}
}

func TestMissingAPIKey(t *testing.T) {
	r := rillock(t, nil, "get", "runs")
	if r.code != exitError || !strings.Contains(r.stderr, "no API key: set RILLOCK_API_KEY") {
		t.Fatalf("code %d, stderr %q", r.code, r.stderr)
	}
}

func TestServerNotRunning(t *testing.T) {
	r := rillock(t, map[string]string{"RILLOCK_API_KEY": "k", "RILLOCK_SERVER": "http://127.0.0.1:1"}, "get", "runs")
	if r.code != exitError || !strings.Contains(r.stderr, "is `rillock server` running?") {
		t.Fatalf("code %d, stderr %q", r.code, r.stderr)
	}
}

func TestKeysGenerate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.yaml")
	r := rillock(t, nil, "keys", "generate", "--name", "jamshid", "--roles", "admin,approver", "--keys-file", path)
	if r.code != 0 {
		t.Fatalf("code %d, stderr %s", r.code, r.stderr)
	}
	key := regexp.MustCompile(`rlk_[A-Za-z0-9_-]+`).FindString(r.stdout)
	if key == "" {
		t.Fatalf("no key in output:\n%s", r.stdout)
	}

	keys, err := auth.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := keys.Authenticate(key)
	if !ok || p.Name != "jamshid" || !p.Has(auth.RoleApprover) {
		t.Fatalf("generated key authenticates as %+v, %v", p, ok)
	}

	r = rillock(t, nil, "keys", "generate", "--name", "x", "--roles", "root")
	if r.code != exitError || !strings.Contains(r.stderr, `unknown role "root"`) {
		t.Fatalf("invalid role: code %d, stderr %q", r.code, r.stderr)
	}
}

func TestFormatAge(t *testing.T) {
	tests := map[time.Duration]string{
		-time.Second:         "0s",
		45 * time.Second:     "45s",
		3 * time.Minute:      "3m",
		5 * time.Hour:        "5h",
		72 * time.Hour:       "3d",
		47*time.Hour + 59*60: "47h",
	}
	for d, want := range tests {
		if got := formatAge(d); got != want {
			t.Errorf("formatAge(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestFormatCost(t *testing.T) {
	tests := map[int64]string{0: "$0.00", 12_000: "$0.012", 1_500_000: "$1.5", 3: "$0.000003"}
	for micro, want := range tests {
		if got := formatCost(micro); got != want {
			t.Errorf("formatCost(%d) = %q, want %q", micro, got, want)
		}
	}
}

func TestShortRunIDs(t *testing.T) {
	env := testServer(t)
	if r := rillock(t, env, "apply", "-f", "testdata/refund.yaml"); r.code != 0 {
		t.Fatalf("apply: %s", r.stderr)
	}
	submit := func() string {
		t.Helper()
		m := runIDPattern.FindStringSubmatch(rillock(t, env, "run", "refund-agent").stdout)
		if m == nil {
			t.Fatal("run did not print an ID")
		}
		return m[1]
	}
	first, second := submit(), submit()

	// Both IDs start with a timestamp, so they share a prefix. That prefix
	// must be rejected as ambiguous rather than silently picking one.
	common := commonPrefix(first, second)
	if r := rillock(t, env, "get", "run", common); r.code != exitError || !strings.Contains(r.stderr, "more than one run ID starts with") {
		t.Fatalf("ambiguous prefix %q: code %d, stderr %q", common, r.code, r.stderr)
	}

	// Enough characters to be unique select the right run, for get and cancel.
	short := first[:len(common)+1]
	if r := rillock(t, env, "get", "run", short); r.code != 0 || !strings.Contains(r.stdout, first) {
		t.Fatalf("get run %s: code %d\n%s%s", short, r.code, r.stdout, r.stderr)
	}
	if r := rillock(t, env, "cancel", short); r.code != 0 || !strings.Contains(r.stdout, first+" cancelled") {
		t.Fatalf("cancel %s: code %d\n%s%s", short, r.code, r.stdout, r.stderr)
	}
	if r := rillock(t, env, "get", "run", second); !strings.Contains(r.stdout, "queued") {
		t.Fatalf("the other run changed:\n%s", r.stdout)
	}

	if r := rillock(t, env, "get", "run", "ffff"); r.code != exitError || !strings.Contains(r.stderr, `no run ID starts with "ffff"`) {
		t.Fatalf("unknown prefix: code %d, stderr %q", r.code, r.stderr)
	}
}

func commonPrefix(a, b string) string {
	i := 0
	for i < len(a) && i < len(b) && a[i] == b[i] {
		i++
	}
	return a[:i]
}

func TestTraceOfACompletedRun(t *testing.T) {
	env, st := testServerWithStore(t)

	tools := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"amount":300}`)
	}))
	t.Cleanup(tools.Close)
	manifest := filepath.Join(t.TempDir(), "agent.yaml")
	if err := os.WriteFile(manifest, fmt.Appendf(nil, `kind: Tool
name: orders_get
description: Look up an order.
endpoint: %s
effect: read_only
---
kind: Agent
name: scripted
model: {provider: fake, name: scripted}
instructions: Help with refunds.
tools: [orders_get]
`, tools.URL), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := rillock(t, env, "apply", "-f", manifest); r.code != 0 {
		t.Fatalf("apply: %s", r.stderr)
	}

	script := `{"script": [
		{"toolCalls": [{"name": "orders_get", "input": {"order_id": "ord_812"}}, {"name": "delete_everything"}]},
		{"text": "Refunded $300."}
	]}`
	m := runIDPattern.FindStringSubmatch(rillock(t, env, "run", "scripted", "--input", script).stdout)
	if m == nil {
		t.Fatal("run did not print an ID")
	}
	w := worker.New(st, rtools.NewHTTPExecutor(rtools.DefaultTimeout), slog.New(slog.DiscardHandler), worker.Config{ID: "test-worker"})
	if worked, err := w.RunOnce(t.Context()); err != nil || !worked {
		t.Fatalf("RunOnce = %v, %v", worked, err)
	}

	r := rillock(t, env, "trace", m[1][:30])
	for _, want := range []string{
		"scripted v1  succeeded",
		"claimed",
		"by test-worker",
		"step 1 ✓",
		"model → orders_get, delete_everything",
		`orders_get {"order_id":"ord_812"} → 200 {"amount":300}`,
		"step 3 ⊘",
		`delete_everything {} → tool "delete_everything" is not available to this agent`,
		"model Refunded $300.",
		"succeeded",
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("trace lacks %q", want)
		}
	}
	if t.Failed() {
		t.Logf("trace output:\n%s", r.stdout)
	}
}

func TestServerRejectsBadWorkerCount(t *testing.T) {
	env := map[string]string{"RILLOCK_DATABASE_URL": "postgres://x@127.0.0.1:1/db", "RILLOCK_KEYS_FILE": "keys.yaml"}
	for _, n := range []string{"-1", "65"} {
		r := rillock(t, env, "server", "--workers", n)
		if r.code != exitUsage || !strings.Contains(r.stderr, "--workers must be between 0 and 64") {
			t.Errorf("--workers %s: code %d, stderr %q", n, r.code, r.stderr)
		}
	}
}
