package importer

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/tasks"
)

// Executors implement tasks.Executor and tasks.ConfigHooks.
var (
	_ tasks.Executor    = importExecutor{}
	_ tasks.ConfigHooks = importExecutor{}
	_ tasks.Executor    = prewarmExecutor{}
	_ tasks.ConfigHooks = prewarmExecutor{}
)

// ImportExecutor is the executor of TypeImport tasks.
func (s *Service) ImportExecutor() tasks.Executor { return importExecutor{s} }

// PrewarmExecutor is the executor of TypePrewarm tasks.
func (s *Service) PrewarmExecutor() tasks.Executor { return prewarmExecutor{s} }

type importExecutor struct{ s *Service }

func (importExecutor) Validate(cfg json.RawMessage) error {
	_, err := validateImport(cfg)
	return err
}

func (e importExecutor) Execute(ctx context.Context, x *tasks.Exec) error {
	cfg, err := validateImport(x.Task.Config)
	if err != nil {
		return err
	}
	return e.s.runImport(ctx, x, cfg)
}

func (e importExecutor) PrepareConfig(ctx context.Context, raw, prev json.RawMessage) (json.RawMessage, error) {
	cfg, err := validateImport(raw)
	if err != nil {
		return nil, err
	}
	if _, err := e.s.authorizeTarget(ctx, cfg.Repository, false); err != nil {
		return nil, err
	}
	if err := e.s.checkURL(ctx, cfg.Source.URL); err != nil {
		return nil, err
	}
	var old *Source
	if len(prev) > 0 {
		var p ImportConfig
		if json.Unmarshal(prev, &p) == nil {
			old = &p.Source
		}
	}
	if err := e.s.sealSource(&cfg.Source, old); err != nil {
		return nil, err
	}
	return json.Marshal(cfg)
}

func (e importExecutor) AuthorizeRun(ctx context.Context, raw json.RawMessage) error {
	var cfg ImportConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("task config: %w", core.ErrInvalid)
	}
	_, err := e.s.authorizeTarget(ctx, cfg.Repository, false)
	return err
}

func (importExecutor) RedactConfig(raw json.RawMessage) json.RawMessage {
	var cfg ImportConfig
	if json.Unmarshal(raw, &cfg) != nil {
		return raw
	}
	redactSource(&cfg.Source)
	b, err := json.Marshal(cfg)
	if err != nil {
		return raw
	}
	return b
}

type prewarmExecutor struct{ s *Service }

func (prewarmExecutor) Validate(cfg json.RawMessage) error {
	_, err := validatePrewarm(cfg)
	return err
}

func (e prewarmExecutor) Execute(ctx context.Context, x *tasks.Exec) error {
	cfg, err := validatePrewarm(x.Task.Config)
	if err != nil {
		return err
	}
	return e.s.runPrewarm(ctx, x, cfg)
}

func (e prewarmExecutor) PrepareConfig(ctx context.Context, raw, prev json.RawMessage) (json.RawMessage, error) {
	cfg, err := validatePrewarm(raw)
	if err != nil {
		return nil, err
	}
	repo, err := e.s.authorizeTarget(ctx, cfg.Repository, true)
	if err != nil {
		return nil, err
	}
	// Lockfiles become coordinates; their content is not stored.
	for i, l := range cfg.Lockfiles {
		if l.Content == "" {
			continue // kept from an earlier save
		}
		parsed, err := ParseLockfile(repo.Format, l.Name, []byte(l.Content))
		if err != nil {
			return nil, err
		}
		cfg.Coordinates = append(cfg.Coordinates, parsed.Coordinates...)
		cfg.Lockfiles[i].Content, cfg.Lockfiles[i].Entries, cfg.Lockfiles[i].Skipped = "", len(parsed.Coordinates), parsed.Skipped
	}
	cfg.Coordinates = dedupe(cfg.Coordinates)
	if len(cfg.Coordinates) > MaxCoordinates {
		return nil, fmt.Errorf("at most %d coordinates per task (got %d): %w", MaxCoordinates, len(cfg.Coordinates), core.ErrInvalid)
	}
	for _, c := range cfg.Coordinates {
		if _, err := parseCoord(repo.Format, c); err != nil {
			return nil, err
		}
	}
	if inv := cfg.Inventory; inv != nil {
		if err := e.s.checkURL(ctx, inv.Source.URL); err != nil {
			return nil, err
		}
		var old *Source
		if len(prev) > 0 {
			var p PrewarmConfig
			if json.Unmarshal(prev, &p) == nil && p.Inventory != nil {
				old = &p.Inventory.Source
			}
		}
		if err := e.s.sealSource(&inv.Source, old); err != nil {
			return nil, err
		}
	}
	return json.Marshal(cfg)
}

func (e prewarmExecutor) AuthorizeRun(ctx context.Context, raw json.RawMessage) error {
	var cfg PrewarmConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("task config: %w", core.ErrInvalid)
	}
	_, err := e.s.authorizeTarget(ctx, cfg.Repository, true)
	return err
}

func (prewarmExecutor) RedactConfig(raw json.RawMessage) json.RawMessage {
	var cfg PrewarmConfig
	if json.Unmarshal(raw, &cfg) != nil {
		return raw
	}
	if cfg.Inventory != nil {
		redactSource(&cfg.Inventory.Source)
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return raw
	}
	return b
}

func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, v := range in {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}
