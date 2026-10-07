package config

import (
	"bytes"
	"errors"
	"flag"
	"strings"
	"testing"
)

// env returns an Env backed by a map, so tests never read the real environment.
func env(vars map[string]string) Env {
	return func(key string) string { return vars[key] }
}

// parse runs Parse on a fresh flag set whose usage output is captured, so
// tests do not print help text.
func parse(t *testing.T, args []string, e Env, settings ...Setting) ([]string, string, error) {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	var out bytes.Buffer
	fs.SetOutput(&out)
	positional, err := Parse(fs, args, e, settings...)
	return positional, out.String(), err
}

func TestPrecedence(t *testing.T) {
	const envURL = "postgres://env@localhost/db"
	const flagURL = "postgres://flag@localhost/db"

	tests := []struct {
		name     string
		args     []string
		env      map[string]string
		wantAddr string
		wantDB   string
	}{
		{
			name:     "defaults",
			env:      map[string]string{"RILLOCK_DATABASE_URL": envURL},
			wantAddr: "127.0.0.1:8080",
			wantDB:   envURL,
		},
		{
			name:     "env overrides defaults",
			env:      map[string]string{"RILLOCK_ADDR": ":9000", "RILLOCK_DATABASE_URL": envURL},
			wantAddr: ":9000",
			wantDB:   envURL,
		},
		{
			name:     "flags override env",
			args:     []string{"--addr", ":9100", "--database-url", flagURL},
			env:      map[string]string{"RILLOCK_ADDR": ":9000", "RILLOCK_DATABASE_URL": envURL},
			wantAddr: ":9100",
			wantDB:   flagURL,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var listen Listen
			var db Database
			if _, _, err := parse(t, tt.args, env(tt.env), &listen, &db); err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if listen.Addr != tt.wantAddr {
				t.Errorf("Addr = %q, want %q", listen.Addr, tt.wantAddr)
			}
			if db.URL != tt.wantDB {
				t.Errorf("Database URL = %q, want %q", db.URL, tt.wantDB)
			}
		})
	}
}

func TestInvalidSettings(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		env      map[string]string
		settings func() []Setting
		wantErr  string
	}{
		{
			name:     "missing database URL",
			settings: func() []Setting { return []Setting{&Database{}} },
			wantErr:  "no database URL",
		},
		{
			name:     "wrong database scheme",
			args:     []string{"--database-url", "mysql://localhost/db"},
			settings: func() []Setting { return []Setting{&Database{}} },
			wantErr:  "scheme must be postgres://",
		},
		{
			name:     "unparsable database URL",
			args:     []string{"--database-url", "postgres://%zz"},
			settings: func() []Setting { return []Setting{&Database{}} },
			wantErr:  "cannot be parsed",
		},
		{
			name:     "empty listen address",
			args:     []string{"--addr", ""},
			settings: func() []Setting { return []Setting{&Listen{}} },
			wantErr:  "--addr must not be empty",
		},
		{
			name:     "missing keys file",
			settings: func() []Setting { return []Setting{&Keys{}} },
			wantErr:  "no API keys file",
		},
		{
			name:     "missing API key",
			settings: func() []Setting { return []Setting{&Client{}} },
			wantErr:  "no API key",
		},
		{
			name:     "invalid server URL",
			args:     []string{"--server", "localhost:8080"},
			env:      map[string]string{"RILLOCK_API_KEY": "k"},
			settings: func() []Setting { return []Setting{&Client{}} },
			wantErr:  "invalid server URL",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := parse(t, tt.args, env(tt.env), tt.settings()...)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestInvalidURLErrorHidesPassword(t *testing.T) {
	_, _, err := parse(t, []string{"--database-url", "postgres://user:s3cret@%zz"}, env(nil), &Database{})
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("error leaks the password: %v", err)
	}
}

func TestCommandsShowOnlyTheirFlags(t *testing.T) {
	_, usage, err := parse(t, []string{"-h"}, env(nil), &Database{})
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("err = %v, want flag.ErrHelp", err)
	}
	if !strings.Contains(usage, "-database-url") {
		t.Errorf("usage does not list -database-url:\n%s", usage)
	}
	if strings.Contains(usage, "-addr") {
		t.Errorf("usage lists -addr, which this command did not register:\n%s", usage)
	}
}

func TestAPIKeyComesOnlyFromEnvironment(t *testing.T) {
	var c Client
	_, _, err := parse(t, []string{"--api-key", "k"}, env(nil), &c)
	if !errors.Is(err, ErrInvalidFlags) || !strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatalf("err = %v, want --api-key to be rejected as an invalid flag", err)
	}

	c = Client{}
	if _, _, err := parse(t, nil, env(map[string]string{"RILLOCK_API_KEY": "k"}), &c); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.APIKey != "k" {
		t.Fatalf("APIKey = %q, want %q", c.APIKey, "k")
	}
}

func TestFlagsAndPositionalArgumentsInAnyOrder(t *testing.T) {
	tests := []struct {
		name           string
		args           []string
		wantPositional string
		wantAddr       string
	}{
		{"flags first", []string{"--addr", ":1", "a", "b"}, "a b", ":1"},
		{"flags last", []string{"a", "b", "--addr", ":1"}, "a b", ":1"},
		{"flags between", []string{"a", "--addr", ":1", "b"}, "a b", ":1"},
		{"everything after -- is positional", []string{"a", "--", "--addr", ":1"}, "a --addr :1", "127.0.0.1:8080"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var listen Listen
			positional, _, err := parse(t, tt.args, env(nil), &listen)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got := strings.Join(positional, " "); got != tt.wantPositional {
				t.Errorf("positional = %q, want %q", got, tt.wantPositional)
			}
			if listen.Addr != tt.wantAddr {
				t.Errorf("Addr = %q, want %q", listen.Addr, tt.wantAddr)
			}
		})
	}
}
