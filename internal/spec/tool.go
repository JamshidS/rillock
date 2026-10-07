package spec

import (
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"slices"
)

// Tool is an HTTP endpoint an agent may call, with the rules for calling it.
type Tool struct {
	Name           string          `json:"name"`
	Description    string          `json:"description"`
	Endpoint       string          `json:"endpoint"`
	Effect         Effect          `json:"effect"`
	InputSchema    json.RawMessage `json:"inputSchema,omitempty"`
	StatusEndpoint string          `json:"statusEndpoint,omitempty"`
	Secrets        []string        `json:"secrets"`
	Policy         []PolicyRule    `json:"policy"`
}

// Effect says what happens if a tool call is repeated, which decides how a run
// recovers after a crash. See the tool effect classes in docs/concepts.md.
type Effect string

// Effect classes.
const (
	EffectReadOnly      Effect = "read_only"
	EffectIdempotent    Effect = "idempotent"
	EffectKeyed         Effect = "keyed"
	EffectCompensatable Effect = "compensatable"
	EffectNonIdempotent Effect = "non_idempotent"
)

var effects = []Effect{EffectReadOnly, EffectIdempotent, EffectKeyed, EffectCompensatable, EffectNonIdempotent}

// PolicyRule decides what happens when the tool is called. When is a CEL
// expression over the call's arguments, for example "args.amount > 100".
type PolicyRule struct {
	When   string `json:"when"`
	Action string `json:"action"`
}

// Policy actions.
const (
	ActionAllow           = "allow"
	ActionDeny            = "deny"
	ActionRequireApproval = "require_approval"
)

var actions = []string{ActionAllow, ActionDeny, ActionRequireApproval}

var (
	// toolName matches what model APIs accept for tool names: they do not
	// allow dots, so tools are named like "payments_refund".
	toolName   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	secretName = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,127}$`)
)

// defaultInputSchema accepts any object, for tools that take no arguments.
var defaultInputSchema = json.RawMessage(`{"type":"object"}`)

// Normalize applies defaults and rewrites the input schema in a canonical
// form (sorted keys, no extra spaces), so key order does not change the hash.
func (t *Tool) Normalize() error {
	if len(t.InputSchema) == 0 {
		t.InputSchema = defaultInputSchema
	}
	var schema any
	if err := json.Unmarshal(t.InputSchema, &schema); err != nil {
		return errors.New("inputSchema: invalid JSON")
	}
	canonical, err := json.Marshal(schema)
	if err != nil {
		return err
	}
	t.InputSchema = canonical

	if t.Secrets == nil {
		t.Secrets = []string{}
	}
	slices.Sort(t.Secrets)
	if t.Policy == nil {
		t.Policy = []PolicyRule{} // rule order matters: first match wins, so no sorting
	}
	return nil
}

// Validate reports every problem with the tool as a *ValidationError.
func (t *Tool) Validate() error {
	var p problems
	if !toolName.MatchString(t.Name) {
		p.add("name: must be 1-64 lowercase letters, digits, or '_', starting with a letter (for example payments_refund)")
	}
	if t.Description == "" {
		p.add("description: required, the model reads it to decide when to use the tool")
	}
	if !validHTTPURL(t.Endpoint) {
		p.add("endpoint: must be an http:// or https:// URL")
	}
	if t.StatusEndpoint != "" && !validHTTPURL(t.StatusEndpoint) {
		p.add("statusEndpoint: must be an http:// or https:// URL")
	}
	if !slices.Contains(effects, t.Effect) {
		p.add("effect: must be one of %v", effects)
	}
	var schema map[string]any
	if json.Unmarshal(t.InputSchema, &schema) != nil || schema["type"] != "object" {
		p.add(`inputSchema: must be a JSON Schema object with "type": "object"`)
	}
	for i, s := range t.Secrets {
		if !secretName.MatchString(s) {
			p.add("secrets: %q must be an environment variable name such as PAYMENTS_API_KEY", s)
		}
		if i > 0 && t.Secrets[i-1] == s {
			p.add("secrets: %q is listed twice", s)
		}
	}
	for i, r := range t.Policy {
		if r.When == "" {
			p.add("policy[%d].when: required", i)
		}
		if !slices.Contains(actions, r.Action) {
			p.add("policy[%d].action: must be one of %v", i, actions)
		}
	}
	return p.err(KindTool, t.Name)
}

func validHTTPURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}
