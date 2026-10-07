package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/jamshids/rillock/internal/spec"
)

// ApplyStatus says what applying one resource did.
type ApplyStatus string

// Apply outcomes.
const (
	ApplyCreated   ApplyStatus = "created"   // first version
	ApplyUpdated   ApplyStatus = "updated"   // spec changed: new version
	ApplyUnchanged ApplyStatus = "unchanged" // same spec as the latest version
)

// ApplyResult reports the outcome for one resource.
type ApplyResult struct {
	Kind    string
	Name    string
	Version int
	Status  ApplyStatus
}

// Apply stores new versions of the given resources in one transaction: either
// every resource is applied, or none is.
//
// Resources must already be validated. Tools are applied before agents, so an
// agent may use a tool defined in the same request.
func (s *Store) Apply(ctx context.Context, resources []spec.Resource) ([]ApplyResult, error) {
	results := make([]ApplyResult, len(resources))
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		for pass := range 2 { // pass 0: tools, pass 1: agents
			for i, r := range resources {
				var res ApplyResult
				var err error
				switch {
				case pass == 0 && r.Tool != nil:
					res, err = applyTool(ctx, tx, r.Tool)
				case pass == 1 && r.Agent != nil:
					res, err = applyAgent(ctx, tx, r.Agent)
				default:
					continue
				}
				if err != nil {
					return err
				}
				results[i] = res
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

func applyTool(ctx context.Context, tx pgx.Tx, t *spec.Tool) (ApplyResult, error) {
	hash, err := spec.Hash(t)
	if err != nil {
		return ApplyResult{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO tools (name) VALUES ($1) ON CONFLICT (name) DO NOTHING`, t.Name); err != nil {
		return ApplyResult{}, fmt.Errorf("tool %s: %w", t.Name, err)
	}
	// Lock the tool's row so two concurrent applies cannot both create version N+1.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM tools WHERE name = $1 FOR UPDATE`, t.Name); err != nil {
		return ApplyResult{}, fmt.Errorf("lock tool %s: %w", t.Name, err)
	}

	latest, latestHash, err := latestVersion(ctx, tx,
		`SELECT version, spec_hash FROM tool_versions WHERE tool_name = $1 ORDER BY version DESC LIMIT 1`, t.Name)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("tool %s: %w", t.Name, err)
	}
	result := ApplyResult{Kind: spec.KindTool, Name: t.Name, Version: latest}
	if latestHash == hash {
		result.Status = ApplyUnchanged
		return result, nil
	}

	result.Version = latest + 1
	if _, err := tx.Exec(ctx, `
		INSERT INTO tool_versions (tool_name, version, effect_class, spec, spec_hash)
		VALUES ($1, $2, $3, $4, $5)`,
		t.Name, result.Version, t.Effect, t, hash); err != nil {
		return ApplyResult{}, fmt.Errorf("tool %s: insert version: %w", t.Name, err)
	}
	result.Status = statusFor(result.Version)
	return result, nil
}

func applyAgent(ctx context.Context, tx pgx.Tx, a *spec.Agent) (ApplyResult, error) {
	if err := checkToolsExist(ctx, tx, a); err != nil {
		return ApplyResult{}, err
	}
	hash, err := spec.Hash(a)
	if err != nil {
		return ApplyResult{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO agents (name) VALUES ($1) ON CONFLICT (name) DO NOTHING`, a.Name); err != nil {
		return ApplyResult{}, fmt.Errorf("agent %s: %w", a.Name, err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM agents WHERE name = $1 FOR UPDATE`, a.Name); err != nil {
		return ApplyResult{}, fmt.Errorf("lock agent %s: %w", a.Name, err)
	}

	latest, latestHash, err := latestVersion(ctx, tx,
		`SELECT version, spec_hash FROM agent_versions WHERE agent_name = $1 ORDER BY version DESC LIMIT 1`, a.Name)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("agent %s: %w", a.Name, err)
	}
	result := ApplyResult{Kind: spec.KindAgent, Name: a.Name, Version: latest}
	if latestHash == hash {
		result.Status = ApplyUnchanged
		return result, nil
	}

	result.Version = latest + 1
	if _, err := tx.Exec(ctx, `
		INSERT INTO agent_versions (agent_name, version, spec, spec_hash)
		VALUES ($1, $2, $3, $4)`,
		a.Name, result.Version, a, hash); err != nil {
		return ApplyResult{}, fmt.Errorf("agent %s: insert version: %w", a.Name, err)
	}
	result.Status = statusFor(result.Version)
	return result, nil
}

// latestVersion runs query (which selects version and spec_hash for name) and
// returns version 0 and an empty hash when nothing has been applied yet.
func latestVersion(ctx context.Context, tx pgx.Tx, query, name string) (int, string, error) {
	var version int
	var hash string
	err := tx.QueryRow(ctx, query, name).Scan(&version, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", nil
	}
	return version, hash, err
}

// checkToolsExist reports tools the agent uses that have never been applied.
func checkToolsExist(ctx context.Context, tx pgx.Tx, a *spec.Agent) error {
	if len(a.Tools) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT name FROM tools WHERE name = ANY($1)`, a.Tools)
	if err != nil {
		return fmt.Errorf("agent %s: look up tools: %w", a.Name, err)
	}
	found, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("agent %s: look up tools: %w", a.Name, err)
	}

	exists := make(map[string]bool, len(found))
	for _, name := range found {
		exists[name] = true
	}
	var missing []string
	for _, name := range a.Tools {
		if !exists[name] {
			missing = append(missing, fmt.Sprintf("tools: %q has not been applied; apply the Tool first or in the same request", name))
		}
	}
	if len(missing) > 0 {
		return &spec.ValidationError{Kind: spec.KindAgent, Name: a.Name, Problems: missing}
	}
	return nil
}

func statusFor(version int) ApplyStatus {
	if version == 1 {
		return ApplyCreated
	}
	return ApplyUpdated
}
