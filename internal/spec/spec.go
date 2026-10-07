// Package spec defines the resources users apply: agents and tools.
//
// A resource arrives as JSON (the CLI converts YAML to JSON). Decode reads it
// strictly: an unknown field such as a misspelled "efect" is an error, not
// silently ignored. Validate checks every field and reports all problems at
// once. Normalize fills in defaults so the stored spec is explicit, and Hash
// identifies a spec so applying the same one twice creates no new version.
//
// This package is pure: no database, no network. Field meanings are in
// docs/concepts.md.
package spec

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Kinds of resources.
const (
	KindAgent = "Agent"
	KindTool  = "Tool"
)

// Resource is one decoded resource: exactly one of Agent or Tool is set.
type Resource struct {
	Agent *Agent
	Tool  *Tool
}

// Kind returns KindAgent or KindTool.
func (r Resource) Kind() string {
	if r.Agent != nil {
		return KindAgent
	}
	return KindTool
}

// Name returns the resource's name.
func (r Resource) Name() string {
	if r.Agent != nil {
		return r.Agent.Name
	}
	return r.Tool.Name
}

// Decode reads one resource from JSON. The "kind" field chooses the type.
// The returned resource is normalized but not yet validated.
func Decode(data []byte) (Resource, error) {
	var head struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return Resource{}, fmt.Errorf("invalid resource: %w", err)
	}

	switch head.Kind {
	case KindAgent:
		var a struct {
			Kind string `json:"kind"`
			Agent
		}
		if err := decodeStrict(data, &a); err != nil {
			return Resource{}, fmt.Errorf("Agent: %w", err)
		}
		a.Normalize()
		return Resource{Agent: &a.Agent}, nil
	case KindTool:
		var t struct {
			Kind string `json:"kind"`
			Tool
		}
		if err := decodeStrict(data, &t); err != nil {
			return Resource{}, fmt.Errorf("Tool: %w", err)
		}
		if err := t.Normalize(); err != nil {
			return Resource{}, fmt.Errorf("Tool %s: %w", t.Name, err)
		}
		return Resource{Tool: &t.Tool}, nil
	case "":
		return Resource{}, errors.New(`invalid resource: missing "kind" (want Agent or Tool)`)
	default:
		return Resource{}, fmt.Errorf("invalid resource: unknown kind %q (want Agent or Tool)", head.Kind)
	}
}

// decodeStrict decodes JSON and rejects unknown fields. Decode has already
// checked with json.Unmarshal that data holds exactly one JSON value.
func decodeStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// Hash returns a stable identifier for a normalized spec. encoding/json writes
// struct fields in declaration order and map keys sorted, so equal specs
// always produce equal bytes.
func Hash(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// ValidationError lists every problem found in one resource.
type ValidationError struct {
	Kind     string
	Name     string
	Problems []string
}

func (e *ValidationError) Error() string {
	name := e.Name
	if name == "" {
		name = "(unnamed)"
	}
	return fmt.Sprintf("%s %s: %s", e.Kind, name, strings.Join(e.Problems, "; "))
}

// problems collects validation messages; err returns nil when there are none.
type problems []string

func (p *problems) add(format string, args ...any) {
	*p = append(*p, fmt.Sprintf(format, args...))
}

func (p problems) err(kind, name string) error {
	if len(p) == 0 {
		return nil
	}
	return &ValidationError{Kind: kind, Name: name, Problems: p}
}
