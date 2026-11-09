package scanning

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// distroSyncInterval is how often the detected releases of APT and RPM
// proxies are refreshed: an APT proxy reveals its suites as clients fetch
// them.
const distroSyncInterval = 10 * time.Minute

// scannable reports whether the components of repo are scanned.
func (s *Service) scannable(ctx context.Context, repo *core.Repository) (bool, error) {
	if _, ok := Ecosystem(repo.Format); ok {
		return true, nil
	}
	if !IsDistroFormat(repo.Format) {
		return false, nil
	}
	if c := repo.Config.Scanning; c != nil && c.Distro != "" {
		return true, nil
	}
	var n int
	err := s.opts.Pool.QueryRow(ctx, `SELECT count(*) FROM repository_distros WHERE repo_id = $1::uuid AND source = $2`,
		repo.ID, DistroFromRemote).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("repository distribution: %w", err)
	}
	return n > 0, nil
}

// RepositoryDistro returns the distribution release the components of an
// APT or RPM repository are scanned in and where it comes from (setting or
// remote); "" when unknown.
func (s *Service) RepositoryDistro(ctx context.Context, repoID string) (ecosystem, source string, err error) {
	err = s.opts.Pool.QueryRow(ctx, `SELECT ecosystem, source FROM repository_distros WHERE repo_id = $1::uuid`, repoID).
		Scan(&ecosystem, &source)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("repository distribution: %w", err)
	}
	return ecosystem, source, nil
}

// SyncDistros records the distribution release of every APT and RPM
// repository (repoID "" for all): the scanning.distro setting, else what a
// proxy's remote reveals (InferDistro). A repository whose release changed
// gets a repository scan, so its findings follow.
func (s *Service) SyncDistros(ctx context.Context, repoID string) error {
	q := `SELECT id::text, format, type, config FROM repositories WHERE format = ANY($1)`
	args := []any{distroFormats}
	if repoID != "" {
		if !isUUID(repoID) {
			return nil
		}
		q += ` AND id = $2::uuid`
		args = append(args, repoID)
	}
	rows, err := s.opts.Pool.Query(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("sync distributions: %w", err)
	}
	type repoRow struct {
		id, format, typ string
		cfg             core.RepoConfig
	}
	repos, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (repoRow, error) {
		var x repoRow
		var raw []byte
		if err := r.Scan(&x.id, &x.format, &x.typ, &raw); err != nil {
			return x, err
		}
		_ = json.Unmarshal(raw, &x.cfg) // a config the scanner cannot read has no release
		return x, nil
	})
	if err != nil {
		return fmt.Errorf("sync distributions: %w", err)
	}
	for _, r := range repos {
		eco, source := "", ""
		switch {
		case r.cfg.Scanning != nil && r.cfg.Scanning.Distro != "":
			eco, source = r.cfg.Scanning.Distro, DistroFromSetting
		case core.RepoType(r.typ) == core.RepoProxy && r.cfg.Proxy != nil:
			var suites []string
			if r.format == "apt" {
				if suites, err = s.cachedSuites(ctx, r.id); err != nil {
					return err
				}
			}
			if eco = InferDistro(r.format, r.cfg.Proxy.RemoteURL, suites); eco != "" {
				source = DistroFromRemote
			}
		}
		if err := s.recordDistro(ctx, r.id, eco, source); err != nil {
			return err
		}
	}
	return nil
}

// cachedSuites lists the suites an APT proxy has cached a Release of.
func (s *Service) cachedSuites(ctx context.Context, repoID string) ([]string, error) {
	rows, err := s.opts.Pool.Query(ctx, `SELECT DISTINCT split_part(path, '/', 2) FROM assets
		WHERE repo_id = $1::uuid AND path LIKE 'dists/%' AND path ~ '^dists/[^/]+/(In)?Release$' ORDER BY 1`, repoID)
	if err != nil {
		return nil, fmt.Errorf("cached suites: %w", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("cached suites: %w", err)
	}
	return out, nil
}

// recordDistro stores the release of a repository ("" removes it) and
// queues a repository scan when it changed.
func (s *Service) recordDistro(ctx context.Context, repoID, eco, source string) error {
	if eco == "" {
		if _, err := s.opts.Pool.Exec(ctx, `DELETE FROM repository_distros WHERE repo_id = $1::uuid`, repoID); err != nil {
			return fmt.Errorf("record distribution: %w", err)
		}
		return nil
	}
	tag, err := s.opts.Pool.Exec(ctx, `INSERT INTO repository_distros (repo_id, ecosystem, source) VALUES ($1::uuid, $2, $3)
		ON CONFLICT (repo_id) DO UPDATE SET ecosystem = EXCLUDED.ecosystem, source = EXCLUDED.source, updated_at = now()
		WHERE repository_distros.ecosystem <> EXCLUDED.ecosystem OR repository_distros.source <> EXCLUDED.source`,
		repoID, eco, source)
	if err != nil {
		return fmt.Errorf("record distribution: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil // unchanged
	}
	s.log.InfoContext(ctx, "scanning: distribution release of repository", "repository_id", repoID, "ecosystem", eco, "source", source)
	if !s.opts.Enabled || s.opts.Jobs == nil {
		return nil
	}
	payload, _ := json.Marshal(jobPayload{RepositoryID: repoID})
	key := repoID + ":distro:" + eco
	if _, err := s.opts.Jobs.Enqueue(ctx, core.Job{Type: JobScanRepository, Key: key, Payload: payload}); err != nil {
		s.log.WarnContext(ctx, "scanning: enqueue repository scan failed", "repository_id", repoID, "error", err)
	}
	return nil
}
