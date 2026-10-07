// Package auth checks API keys and the roles they grant.
//
// The server never stores API keys, only their SHA-256 hashes, in a keys file:
//
//	keys:
//	  - name: jamshid
//	    roles: [admin, approver]
//	    sha256: 3b0c...e9
//
// `rillock keys generate` creates a key and prints the entry to add.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sync/atomic"

	"go.yaml.in/yaml/v3"
)

// Role is a permission granted to a key.
type Role string

// Roles, from most to least powerful. Every role may also read (see Principal.Has).
const (
	RoleAdmin    Role = "admin"    // apply resources, submit and cancel runs
	RoleApprover Role = "approver" // approve or deny risky tool calls
	RoleViewer   Role = "viewer"   // read runs and events
)

var validRoles = []Role{RoleAdmin, RoleApprover, RoleViewer}

// Principal is who is making a request: the name of the key and its roles.
type Principal struct {
	Name  string
	Roles []Role
}

// Has reports whether p may act with role r. Any role implies RoleViewer:
// someone who may change or approve things may also see them.
func (p Principal) Has(r Role) bool {
	if r == RoleViewer {
		return len(p.Roles) > 0
	}
	return slices.Contains(p.Roles, r)
}

// Keys authenticates API keys. The zero value accepts no keys.
type Keys struct {
	byHash map[string]Principal
}

// keyPrefix makes Rillock keys recognizable, for example by secret scanners.
const keyPrefix = "rlk_"

// Generate returns a new random API key.
func Generate() (string, error) {
	b := make([]byte, 32) // 256 bits: impossible to guess
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate key: %w", err)
	}
	return keyPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// Hash returns the hex SHA-256 of key, the form stored in the keys file.
//
// A fast hash is fine here, unlike for passwords: keys are 256-bit random
// values, so trying guesses against a leaked hash is hopeless.
func Hash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// Authenticate returns the principal for key, or false if the key is unknown.
//
// The key is hashed before the lookup. Comparing hashes instead of the keys
// themselves also means lookup timing reveals nothing useful about the key.
func (k *Keys) Authenticate(key string) (Principal, bool) {
	if key == "" {
		return Principal{}, false
	}
	p, ok := k.byHash[Hash(key)]
	return p, ok
}

// Entry is one key in the keys file.
type Entry struct {
	Name   string `yaml:"name"`
	Roles  []Role `yaml:"roles"`
	SHA256 string `yaml:"sha256"`
}

type file struct {
	Keys []Entry `yaml:"keys"`
}

// Load reads and validates a keys file.
func Load(path string) (*Keys, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the path comes from the server operator's own flag
	if err != nil {
		return nil, fmt.Errorf("read keys file: %w", err)
	}
	keys, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("keys file %s: %w", path, err)
	}
	return keys, nil
}

var (
	namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
	hashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Parse validates the contents of a keys file.
func Parse(data []byte) (*Keys, error) {
	var f file
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("invalid YAML: %w", err)
	}
	if len(f.Keys) == 0 {
		return nil, errors.New("no keys defined")
	}

	keys := &Keys{byHash: make(map[string]Principal, len(f.Keys))}
	names := make(map[string]bool, len(f.Keys))
	for i, e := range f.Keys {
		if err := e.Validate(); err != nil {
			return nil, fmt.Errorf("key %d (%q): %w", i+1, e.Name, err)
		}
		if names[e.Name] {
			return nil, fmt.Errorf("key %d: duplicate name %q", i+1, e.Name)
		}
		if _, dup := keys.byHash[e.SHA256]; dup {
			return nil, fmt.Errorf("key %d (%q): the same key is listed twice", i+1, e.Name)
		}
		names[e.Name] = true
		keys.byHash[e.SHA256] = Principal{Name: e.Name, Roles: e.Roles}
	}
	return keys, nil
}

// Validate checks the entry's name, roles, and hash.
func (e Entry) Validate() error {
	if !namePattern.MatchString(e.Name) {
		return errors.New("name must be 1-63 lowercase letters, digits, '.', '_' or '-'")
	}
	if len(e.Roles) == 0 {
		return errors.New("at least one role is required")
	}
	for _, r := range e.Roles {
		if !slices.Contains(validRoles, r) {
			return fmt.Errorf("unknown role %q (want admin, approver, or viewer)", r)
		}
	}
	if !hashPattern.MatchString(e.SHA256) {
		return errors.New("sha256 must be 64 lowercase hex characters (use `rillock keys generate`)")
	}
	return nil
}

// AddToFile appends e to the keys file at path, creating the file if needed.
// The result is validated before writing, so a duplicate name or an invalid
// entry leaves the file unchanged.
func AddToFile(path string, e Entry) error {
	// Catches typos such as "keys.yaml," that would silently create a
	// second file the server never reads.
	if ext := filepath.Ext(path); ext != ".yaml" && ext != ".yml" {
		return fmt.Errorf("keys file %q must end in .yaml or .yml", path)
	}
	var f file
	data, err := os.ReadFile(path) //nolint:gosec // the path comes from the operator's own flag
	switch {
	case errors.Is(err, os.ErrNotExist):
		// A new file.
	case err != nil:
		return fmt.Errorf("read keys file: %w", err)
	default:
		if err := yaml.Unmarshal(data, &f); err != nil {
			return fmt.Errorf("keys file %s: invalid YAML: %w", path, err)
		}
	}

	for _, existing := range f.Keys {
		if existing.Name == e.Name {
			return fmt.Errorf("%s already has a key named %q; choose another --name", path, e.Name)
		}
	}
	f.Keys = append(f.Keys, e)
	out, err := yaml.Marshal(f)
	if err != nil {
		return err
	}
	if _, err := Parse(out); err != nil {
		return fmt.Errorf("keys file %s: %w", path, err)
	}
	// 0600: only the owner may read the file. It holds hashes, not keys, but
	// there is no reason for anyone else to read it.
	return os.WriteFile(path, out, 0o600)
}

// ReloadableKeys holds keys that can be replaced while requests are running,
// so the server can pick up a changed keys file without restarting.
//
// Create it with LoadReloadable. It is safe for concurrent use: every request
// calls Authenticate while Reload may swap the keys at any moment.
type ReloadableKeys struct {
	path string
	// atomic.Pointer lets readers load the current keys without locking.
	// Reload builds a complete new *Keys first and swaps it in one step, so a
	// request sees either the old keys or the new ones, never a mix.
	current atomic.Pointer[Keys]
}

// LoadReloadable reads the keys file at path and returns keys that Reload can
// later refresh from the same file.
func LoadReloadable(path string) (*ReloadableKeys, error) {
	keys, err := Load(path)
	if err != nil {
		return nil, err
	}
	r := &ReloadableKeys{path: path}
	r.current.Store(keys)
	return r, nil
}

// Authenticate checks key against the current keys. A ReloadableKeys that was
// not created with LoadReloadable accepts nothing rather than crashing.
func (r *ReloadableKeys) Authenticate(key string) (Principal, bool) {
	keys := r.current.Load()
	if keys == nil {
		return Principal{}, false
	}
	return keys.Authenticate(key)
}

// Reload reads the keys file again. If the file is missing or invalid, the
// current keys stay in use and the error is returned: a typo in the file must
// not lock every user out.
func (r *ReloadableKeys) Reload() error {
	keys, err := Load(r.path)
	if err != nil {
		return err
	}
	r.current.Store(keys)
	return nil
}

// Count returns how many keys are loaded, for log messages.
func (r *ReloadableKeys) Count() int {
	keys := r.current.Load()
	if keys == nil {
		return 0
	}
	return len(keys.byHash)
}
