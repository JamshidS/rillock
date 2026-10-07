// Package config reads Rillock's settings from command-line flags and
// environment variables.
//
// Settings come in small groups (Listen, Database, Keys, Client). Each command
// registers only the groups it needs, so `rillock migrate -h` lists only the
// flags migrate uses. For every setting, the first source found wins:
//
//  1. a command-line flag      --database-url=...
//  2. an environment variable  RILLOCK_DATABASE_URL=...
//  3. a built-in default
package config

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
)

// Env looks up an environment variable. os.Getenv satisfies it; tests pass a
// map-backed function so they never read or change the real environment.
type Env func(key string) string

// Setting is a group of related settings that a command can register.
//
// The methods are unexported, so only this package can define settings. That
// keeps every flag name and environment variable documented in one place.
type Setting interface {
	register(fs *flag.FlagSet, env Env)
	validate() error
}

// Parse registers settings on fs, parses args, validates the result, and
// returns the positional (non-flag) arguments. Commands may add their own
// flags to fs before calling Parse.
//
// Unlike fs.Parse alone, flags may come after positional arguments, as in
// `rillock run refund-agent --input "..."`. Everything after "--" is positional.
//
// With -h or --help, Parse returns flag.ErrHelp after printing usage.
func Parse(fs *flag.FlagSet, args []string, env Env, settings ...Setting) ([]string, error) {
	for _, s := range settings {
		s.register(fs, env)
	}
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return nil, err
	}
	for _, s := range settings {
		if err := s.validate(); err != nil {
			return nil, err
		}
	}
	return positional, nil
}

// ErrInvalidFlags wraps errors from the flag package, which has already
// printed the problem and the usage text by the time Parse returns.
var ErrInvalidFlags = errors.New("invalid flags")

// parseInterspersed calls fs.Parse repeatedly. fs.Parse stops at the first
// positional argument; we set that one aside and parse the rest again.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, fmt.Errorf("%w: %w", ErrInvalidFlags, err)
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		// fs.Parse also stops after "--", which it consumes: then everything
		// left is positional.
		if consumed := len(args) - len(rest); consumed > 0 && args[consumed-1] == "--" {
			return append(positional, rest...), nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// Listen is the address the server listens on.
type Listen struct {
	Addr string
}

func (l *Listen) register(fs *flag.FlagSet, env Env) {
	// The environment value becomes the flag's default, so a flag given on the
	// command line overrides it. That is the whole precedence rule.
	fs.StringVar(&l.Addr, "addr", envOr(env, "RILLOCK_ADDR", "127.0.0.1:8080"),
		"listen address (env RILLOCK_ADDR)")
}

func (l *Listen) validate() error {
	if l.Addr == "" {
		return errors.New("--addr must not be empty")
	}
	return nil
}

// ErrNoDatabase is returned when a command needs a database URL and none is set.
var ErrNoDatabase = errors.New("no database URL: set --database-url or RILLOCK_DATABASE_URL")

// Database is the PostgreSQL connection URL. It is required.
type Database struct {
	URL string
}

func (d *Database) register(fs *flag.FlagSet, env Env) {
	fs.StringVar(&d.URL, "database-url", env("RILLOCK_DATABASE_URL"),
		"PostgreSQL URL (env RILLOCK_DATABASE_URL)")
}

func (d *Database) validate() error {
	if d.URL == "" {
		return ErrNoDatabase
	}
	u, err := url.Parse(d.URL)
	if err != nil {
		// Do not include the URL or the parse error: both can contain the password.
		return errors.New("invalid database URL: cannot be parsed")
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return fmt.Errorf("invalid database URL: scheme must be postgres://, got %q", u.Scheme)
	}
	return nil
}

// Keys is the path of the API keys file the server authenticates requests with.
type Keys struct {
	File string
}

func (k *Keys) register(fs *flag.FlagSet, env Env) {
	fs.StringVar(&k.File, "keys-file", env("RILLOCK_KEYS_FILE"),
		"API keys file, see `rillock keys generate` (env RILLOCK_KEYS_FILE)")
}

func (k *Keys) validate() error {
	if k.File == "" {
		return errors.New("no API keys file: set --keys-file or RILLOCK_KEYS_FILE (create one with `rillock keys generate`)")
	}
	return nil
}

// Client holds what CLI commands need to talk to a server.
//
// The API key is read only from the environment, never from a flag: flags end
// up in shell history and are visible to other users in `ps` output.
type Client struct {
	Server string
	APIKey string
}

func (c *Client) register(fs *flag.FlagSet, env Env) {
	fs.StringVar(&c.Server, "server", envOr(env, "RILLOCK_SERVER", "http://127.0.0.1:8080"),
		"server URL (env RILLOCK_SERVER)")
	c.APIKey = env("RILLOCK_API_KEY")
}

func (c *Client) validate() error {
	u, err := url.Parse(c.Server)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("invalid server URL %q: want http://host:port or https://host", c.Server)
	}
	if c.APIKey == "" {
		return errors.New("no API key: set RILLOCK_API_KEY (create one with `rillock keys generate`)")
	}
	return nil
}

func envOr(env Env, key, fallback string) string {
	if v := env(key); v != "" {
		return v
	}
	return fallback
}
