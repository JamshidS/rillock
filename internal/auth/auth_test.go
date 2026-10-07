package auth

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func keysFile(entries ...string) []byte {
	return []byte("keys:\n" + strings.Join(entries, ""))
}

func entry(name, roles, hash string) string {
	return fmt.Sprintf("  - name: %s\n    roles: [%s]\n    sha256: %s\n", name, roles, hash)
}

func TestGenerateAndAuthenticate(t *testing.T) {
	key, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key, "rlk_") || len(key) < 40 {
		t.Fatalf("key %q does not look like a Rillock key", key)
	}

	keys, err := Parse(keysFile(entry("jamshid", "admin, approver", Hash(key))))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	p, ok := keys.Authenticate(key)
	if !ok || p.Name != "jamshid" {
		t.Fatalf("Authenticate = %+v, %v; want jamshid, true", p, ok)
	}
	for _, bad := range []string{"", "rlk_wrong", key + "x", Hash(key)} {
		if _, ok := keys.Authenticate(bad); ok {
			t.Errorf("Authenticate(%q) succeeded, want failure", bad)
		}
	}
}

func TestGenerateIsRandom(t *testing.T) {
	a, _ := Generate()
	b, _ := Generate()
	if a == b {
		t.Fatal("two generated keys are equal")
	}
}

func TestRoles(t *testing.T) {
	tests := []struct {
		roles []Role
		check Role
		want  bool
	}{
		{[]Role{RoleAdmin}, RoleAdmin, true},
		{[]Role{RoleAdmin}, RoleApprover, false}, // admin does not imply approver
		{[]Role{RoleAdmin}, RoleViewer, true},    // every role may read
		{[]Role{RoleApprover}, RoleViewer, true},
		{[]Role{RoleViewer}, RoleAdmin, false},
		{nil, RoleViewer, false},
	}
	for _, tt := range tests {
		p := Principal{Name: "k", Roles: tt.roles}
		if got := p.Has(tt.check); got != tt.want {
			t.Errorf("roles %v: Has(%s) = %v, want %v", tt.roles, tt.check, got, tt.want)
		}
	}
}

func TestParseRejectsInvalidFiles(t *testing.T) {
	good := Hash("k1")
	tests := []struct {
		name    string
		data    []byte
		wantErr string
	}{
		{"not YAML", []byte("keys: [unclosed"), "invalid YAML"},
		{"no keys", []byte("keys: []"), "no keys defined"},
		{"bad name", keysFile(entry("Has Spaces", "admin", good)), "name must be"},
		{"no roles", keysFile(entry("a", "", good)), "at least one role"},
		{"unknown role", keysFile(entry("a", "root", good)), `unknown role "root"`},
		{"plain key instead of hash", keysFile(entry("a", "admin", "rlk_abc")), "sha256 must be"},
		{"duplicate name", keysFile(entry("a", "admin", good), entry("a", "viewer", Hash("k2"))), "duplicate name"},
		{"same key twice", keysFile(entry("a", "admin", good), entry("b", "viewer", good)), "listed twice"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(tt.data)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.yaml")
	if err := os.WriteFile(path, keysFile(entry("a", "viewer", Hash("k"))), 0o600); err != nil {
		t.Fatal(err)
	}
	keys, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := keys.Authenticate("k"); !ok {
		t.Fatal("key from file not accepted")
	}

	if _, err := Load(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Fatal("Load of a missing file succeeded")
	}
}

func TestAddToFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.yaml")

	if err := AddToFile(path, Entry{Name: "a", Roles: []Role{RoleAdmin}, SHA256: Hash("k1")}); err != nil {
		t.Fatalf("first AddToFile: %v", err)
	}
	if err := AddToFile(path, Entry{Name: "b", Roles: []Role{RoleViewer}, SHA256: Hash("k2")}); err != nil {
		t.Fatalf("second AddToFile: %v", err)
	}
	keys, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"k1", "k2"} {
		if _, ok := keys.Authenticate(k); !ok {
			t.Errorf("key %s missing after AddToFile", k)
		}
	}

	before, _ := os.ReadFile(path)
	err = AddToFile(path, Entry{Name: "a", Roles: []Role{RoleViewer}, SHA256: Hash("k3")})
	if err == nil || !strings.Contains(err.Error(), `already has a key named "a"`) {
		t.Fatalf("err = %v, want a clear duplicate-name error", err)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatal("a rejected entry changed the file")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("file permissions = %o, want 600", perm)
	}
}

func TestAddToFileRejectsOddFileNames(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"keys.yaml,", "keys", "keys.txt"} {
		path := filepath.Join(dir, name)
		err := AddToFile(path, Entry{Name: "a", Roles: []Role{RoleAdmin}, SHA256: Hash("k")})
		if err == nil || !strings.Contains(err.Error(), "must end in .yaml or .yml") {
			t.Errorf("%s: err = %v, want an extension error", name, err)
		}
		if _, statErr := os.Stat(path); statErr == nil {
			t.Errorf("%s: file was created anyway", name)
		}
	}
}

func writeKeys(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReloadPicksUpNewKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.yaml")
	writeKeys(t, path, keysFile(entry("a", "admin", Hash("old"))))

	keys, err := LoadReloadable(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := keys.Authenticate("new"); ok {
		t.Fatal("new key accepted before it was added")
	}

	writeKeys(t, path, keysFile(entry("a", "admin", Hash("old")), entry("b", "viewer", Hash("new"))))
	if err := keys.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if p, ok := keys.Authenticate("new"); !ok || p.Name != "b" {
		t.Fatalf("after Reload: Authenticate = %+v, %v; want b, true", p, ok)
	}
	if keys.Count() != 2 {
		t.Fatalf("Count = %d, want 2", keys.Count())
	}
}

func TestReloadKeepsOldKeysWhenFileIsBroken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.yaml")
	writeKeys(t, path, keysFile(entry("a", "admin", Hash("old"))))
	keys, err := LoadReloadable(path)
	if err != nil {
		t.Fatal(err)
	}

	for name, data := range map[string][]byte{
		"invalid YAML": []byte("keys: [unclosed"),
		"invalid role": keysFile(entry("a", "root", Hash("old"))),
	} {
		t.Run(name, func(t *testing.T) {
			writeKeys(t, path, data)
			if err := keys.Reload(); err == nil {
				t.Fatal("Reload accepted a broken file")
			}
			if _, ok := keys.Authenticate("old"); !ok {
				t.Fatal("a failed Reload removed the working keys")
			}
		})
	}

	t.Run("file deleted", func(t *testing.T) {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := keys.Reload(); err == nil {
			t.Fatal("Reload of a missing file succeeded")
		}
		if _, ok := keys.Authenticate("old"); !ok {
			t.Fatal("a failed Reload removed the working keys")
		}
	})
}

// TestReloadWhileAuthenticating must run with -race (make test does). Replace
// the atomic.Pointer in ReloadableKeys with a plain *Keys field to see the race
// detector report the bug this prevents.
func TestReloadWhileAuthenticating(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.yaml")
	writeKeys(t, path, keysFile(entry("a", "admin", Hash("k"))))
	keys, err := LoadReloadable(path)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 10 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
					if _, ok := keys.Authenticate("k"); !ok {
						t.Error("key rejected during a reload")
						return
					}
				}
			}
		})
	}
	for range 100 {
		if err := keys.Reload(); err != nil {
			t.Error(err)
			break
		}
	}
	close(stop)
	wg.Wait()
}

func TestZeroReloadableKeysDeniesInsteadOfCrashing(t *testing.T) {
	var keys ReloadableKeys
	if _, ok := keys.Authenticate("anything"); ok {
		t.Fatal("zero value accepted a key")
	}
}
