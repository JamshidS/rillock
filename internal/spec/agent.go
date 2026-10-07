package spec

import (
	"math"
	"regexp"
	"slices"
	"time"
)

// Agent is a model, its instructions, the tools it may use, and its limits.
type Agent struct {
	Name         string   `json:"name"`
	Model        Model    `json:"model"`
	Instructions string   `json:"instructions"`
	Tools        []string `json:"tools"`
	Limits       Limits   `json:"limits"`
}

// Model selects the provider and model an agent uses.
type Model struct {
	Provider string `json:"provider"` // "anthropic", or "fake" for tests and offline demos
	Name     string `json:"name"`
}

// Limits stop a run that goes on too long or costs too much.
type Limits struct {
	MaxSteps    int     `json:"maxSteps"`
	MaxCostUSD  float64 `json:"maxCostUSD"`
	MaxDuration string  `json:"maxDuration"` // a Go duration such as "5m"
}

// Defaults applied when a limit is not set. Every run has limits; there is no
// "unlimited".
const (
	DefaultMaxSteps    = 50
	DefaultMaxCostUSD  = 1.0
	DefaultMaxDuration = "10m"
)

// Providers an agent may use.
var providers = []string{"anthropic", "fake"}

// agentName is a DNS-label-like name: lowercase letters, digits, and dashes.
var agentName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

const maxInstructions = 100_000

// Normalize applies defaults and puts tools in a stable order, so that two
// specs meaning the same thing have the same hash.
func (a *Agent) Normalize() {
	if a.Limits.MaxSteps == 0 {
		a.Limits.MaxSteps = DefaultMaxSteps
	}
	if a.Limits.MaxCostUSD == 0 {
		a.Limits.MaxCostUSD = DefaultMaxCostUSD
	}
	if a.Limits.MaxDuration == "" {
		a.Limits.MaxDuration = DefaultMaxDuration
	}
	if a.Tools == nil {
		a.Tools = []string{} // stored as [] rather than null
	}
	slices.Sort(a.Tools)
}

// Validate reports every problem with the agent as a *ValidationError.
func (a *Agent) Validate() error {
	var p problems
	if !agentName.MatchString(a.Name) {
		p.add("name: must be 1-63 lowercase letters, digits, or '-', starting and ending with a letter or digit")
	}
	if !slices.Contains(providers, a.Model.Provider) {
		p.add("model.provider: must be one of %v", providers)
	}
	if a.Model.Name == "" {
		p.add("model.name: required")
	}
	if a.Instructions == "" {
		p.add("instructions: required")
	}
	if len(a.Instructions) > maxInstructions {
		p.add("instructions: longer than %d characters", maxInstructions)
	}
	for i, tool := range a.Tools {
		if !toolName.MatchString(tool) {
			p.add("tools: %q is not a valid tool name", tool)
		}
		if i > 0 && a.Tools[i-1] == tool { // tools are sorted, so duplicates are adjacent
			p.add("tools: %q is listed twice", tool)
		}
	}
	if a.Limits.MaxSteps < 1 || a.Limits.MaxSteps > 1000 {
		p.add("limits.maxSteps: must be between 1 and 1000")
	}
	if a.Limits.MaxCostUSD <= 0 || a.Limits.MaxCostUSD > 1000 {
		p.add("limits.maxCostUSD: must be more than 0 and at most 1000")
	}
	if d, err := time.ParseDuration(a.Limits.MaxDuration); err != nil || d < time.Second || d > 24*time.Hour {
		p.add(`limits.maxDuration: must be a duration between 1s and 24h, such as "5m"`)
	}
	return p.err(KindAgent, a.Name)
}

// MaxCostMicroUSD returns the cost limit in micro-dollars (1 USD = 1,000,000),
// the unit runs are charged in. Money is counted in whole numbers so repeated
// additions never accumulate floating-point error.
func (l Limits) MaxCostMicroUSD() int64 {
	return int64(math.Round(l.MaxCostUSD * 1_000_000))
}
