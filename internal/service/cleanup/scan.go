package cleanup

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// packageBatch is the number of packages evaluated (and, when running,
// deleted from) per batch; a run checkpoints after every batch.
const packageBatch = 200

// position is where a scan resumes: the first repository with a name >=
// Repo, and within it the packages after (Namespace, Name).
type position struct {
	Repo      string `json:"repo"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Started   bool   `json:"started"` // a package of Repo was completed
}

// Candidate is a version a policy deletes (schema CleanupCandidate).
type Candidate struct {
	ID         string    `json:"id"`
	Repository string    `json:"repository"`
	Format     string    `json:"format"`
	Namespace  string    `json:"namespace"`
	Name       string    `json:"name"`
	Version    string    `json:"version"`
	SizeBytes  int64     `json:"size_bytes"`
	CreatedAt  time.Time `json:"created_at"`
	// PublishedAt equals CreatedAt (the field name PublishedComponent uses).
	PublishedAt      time.Time  `json:"published_at"`
	LastDownloadedAt *time.Time `json:"last_downloaded_at"`
	DownloadCount    int64      `json:"download_count"`
}

// Coordinates renders "namespace/name@version".
func (c Candidate) Coordinates() string {
	return core.Coordinates{Namespace: c.Namespace, Name: c.Name, Version: c.Version}.String()
}

type scanHooks struct {
	// candidate is called for every selected version.
	candidate func(repo *core.Repository, c Candidate) error
	// held is called for versions kept only because of quarantine or legal hold.
	held func(repo *core.Repository)
	// batch is called after every batch of packages with the resume position
	// and the fraction of all repositories done.
	batch func(pos position, fraction float64) error
}

// scan evaluates the policy over repos (sorted by name) from pos.
func (s *Service) scan(ctx context.Context, r *rules, repos []*core.Repository, now time.Time, pos position, h scanHooks) error {
	for i, repo := range repos {
		if repo.Name < pos.Repo {
			continue
		}
		after := position{Repo: repo.Name}
		if repo.Name == pos.Repo {
			after = pos
		}
		var total int64
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM (SELECT DISTINCT namespace, name FROM components WHERE repo_id = $1) x`,
			repo.ID).Scan(&total); err != nil {
			return fmt.Errorf("count packages of %s: %w", repo.Name, err)
		}
		var done int64
		if after.Started {
			if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM (SELECT DISTINCT namespace, name FROM components
WHERE repo_id = $1 AND (namespace, name) <= ($2, $3)) x`, repo.ID, after.Namespace, after.Name).Scan(&done); err != nil {
				return fmt.Errorf("count packages of %s: %w", repo.Name, err)
			}
		}
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			pkgs, err := s.nextPackages(ctx, repo.ID, after)
			if err != nil {
				return err
			}
			if len(pkgs) == 0 {
				break
			}
			if err := s.evaluate(ctx, r, repo, pkgs, now, h); err != nil {
				return err
			}
			last := pkgs[len(pkgs)-1]
			after = position{Repo: repo.Name, Namespace: last[0], Name: last[1], Started: true}
			done += int64(len(pkgs))
			if h.batch != nil {
				frac := float64(i) / float64(len(repos))
				if total > 0 {
					frac += min(float64(done)/float64(total), 1) / float64(len(repos))
				}
				if err := h.batch(after, frac); err != nil {
					return err
				}
			}
			if len(pkgs) < packageBatch {
				break
			}
		}
	}
	return nil
}

func (s *Service) nextPackages(ctx context.Context, repoID string, after position) ([][2]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT namespace, name FROM components
WHERE repo_id = $1 AND (NOT $2 OR (namespace, name) > ($3, $4))
GROUP BY namespace, name ORDER BY namespace, name LIMIT $5`, repoID, after.Started, after.Namespace, after.Name, packageBatch)
	if err != nil {
		return nil, fmt.Errorf("list packages: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) ([2]string, error) {
		var p [2]string
		err := r.Scan(&p[0], &p[1])
		return p, err
	})
}

// evaluate loads every version of pkgs (a contiguous, sorted range) and
// applies the rules package by package.
func (s *Service) evaluate(ctx context.Context, r *rules, repo *core.Repository, pkgs [][2]string, now time.Time, h scanHooks) error {
	first, last := pkgs[0], pkgs[len(pkgs)-1]
	rows, err := s.pool.Query(ctx, `
SELECT c.id::text, c.namespace, c.name, c.version, c.created_at, c.last_downloaded_at, c.download_count, c.quarantined,
	coalesce(c.attributes->'legal_hold' = 'true'::jsonb, false),
	coalesce((SELECT sum(a.size) FROM assets a WHERE a.component_id = c.id), 0)::bigint
FROM components c
WHERE c.repo_id = $1 AND (c.namespace, c.name) >= ($2, $3) AND (c.namespace, c.name) <= ($4, $5)
ORDER BY c.namespace, c.name, c.created_at DESC, c.id DESC`, repo.ID, first[0], first[1], last[0], last[1])
	if err != nil {
		return fmt.Errorf("list versions of %s: %w", repo.Name, err)
	}
	versions, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (version, error) {
		var v version
		err := row.Scan(&v.ID, &v.Namespace, &v.Name, &v.Version, &v.CreatedAt, &v.LastDownloadedAt, &v.DownloadCount,
			&v.Quarantined, &v.LegalHold, &v.Size)
		return v, err
	})
	if err != nil {
		return fmt.Errorf("list versions of %s: %w", repo.Name, err)
	}
	for start := 0; start < len(versions); {
		end := start + 1
		for end < len(versions) && versions[end].Namespace == versions[start].Namespace && versions[end].Name == versions[start].Name {
			end++
		}
		pkg := versions[start:end]
		for i, v := range r.decide(repo.Format, pkg, now) {
			switch v {
			case heldBack:
				if h.held != nil {
					h.held(repo)
				}
			case deleteIt:
				c := pkg[i]
				cand := Candidate{ID: c.ID, Repository: repo.Name, Format: repo.Format, Namespace: c.Namespace, Name: c.Name,
					Version: c.Version, SizeBytes: c.Size, CreatedAt: c.CreatedAt.UTC(), PublishedAt: c.CreatedAt.UTC(),
					DownloadCount: c.DownloadCount}
				if c.LastDownloadedAt != nil {
					t := c.LastDownloadedAt.UTC()
					cand.LastDownloadedAt = &t
				}
				if err := h.candidate(repo, cand); err != nil {
					return err
				}
			}
		}
		start = end
	}
	return nil
}

// RepoSummary is the per-repository part of a preview or run result.
type RepoSummary struct {
	Repository     string `json:"repository"`
	ComponentCount int64  `json:"component_count"`
	Bytes          int64  `json:"bytes"`
}

// Preview is the dry run of a policy (schema CleanupPreview).
type Preview struct {
	ComponentCount int64         `json:"component_count"`
	Bytes          int64         `json:"bytes"`
	HeldCount      int64         `json:"held_count"`
	Repositories   []RepoSummary `json:"repositories"`
	// Sample lists the first candidates (by repository and package); Truncated
	// is set when there are more. The CSV export lists every one.
	Sample       []Candidate `json:"sample"`
	Truncated    bool        `json:"truncated"`
	EvaluatedAt  time.Time   `json:"evaluated_at"`
	PreviewToken string      `json:"preview_token"`
	ExpiresAt    time.Time   `json:"expires_at"`
}

// Default and maximum number of candidates in Preview.Sample.
const (
	DefaultSample = 100
	MaxSample     = 1000
)

// plan validates in and resolves the repositories it cleans.
func (s *Service) plan(ctx context.Context, in *Input) (*rules, []*core.Repository, error) {
	r, err := s.validate(in)
	if err != nil {
		return nil, nil, err
	}
	repos, err := s.effective(ctx, in)
	if err != nil {
		return nil, nil, err
	}
	return r, repos, nil
}

// Preview evaluates in (a saved or unsaved policy) without deleting
// anything and issues a preview token for saving exactly these rules.
// sample bounds Preview.Sample (0 = DefaultSample). The caller authorizes.
func (s *Service) Preview(ctx context.Context, in Input, sample int) (*Preview, error) {
	r, repos, err := s.plan(ctx, &in)
	if err != nil {
		return nil, err
	}
	switch {
	case sample <= 0:
		sample = DefaultSample
	case sample > MaxSample:
		sample = MaxSample
	}
	now := s.opts.Now().UTC()
	p := &Preview{EvaluatedAt: now, Sample: []Candidate{}, Repositories: []RepoSummary{}}
	byRepo := map[string]*RepoSummary{}
	for _, repo := range repos {
		p.Repositories = append(p.Repositories, RepoSummary{Repository: repo.Name})
	}
	for i := range p.Repositories {
		byRepo[p.Repositories[i].Repository] = &p.Repositories[i]
	}
	err = s.scan(ctx, r, repos, now, position{}, scanHooks{
		candidate: func(repo *core.Repository, c Candidate) error {
			p.ComponentCount++
			p.Bytes += c.SizeBytes
			rs := byRepo[repo.Name]
			rs.ComponentCount++
			rs.Bytes += c.SizeBytes
			if len(p.Sample) < sample {
				p.Sample = append(p.Sample, c)
			} else {
				p.Truncated = true
			}
			return nil
		},
		held: func(*core.Repository) { p.HeldCount++ },
	})
	if err != nil {
		return nil, err
	}
	token := newToken()
	p.PreviewToken = token
	p.ExpiresAt = now.Add(s.opts.PreviewTTL)
	if _, err := s.pool.Exec(ctx, `INSERT INTO cleanup_previews (token_hash, fingerprint, actor, expires_at) VALUES ($1, $2, $3, $4)`,
		hashToken(token), fingerprint(&in), core.PrincipalFrom(ctx).Name, p.ExpiresAt); err != nil {
		return nil, fmt.Errorf("store preview token: %w", err)
	}
	return p, nil
}

// CSVHeader is the first line of WriteCSV.
var CSVHeader = []string{"repository", "format", "namespace", "name", "version", "size_bytes", "created_at",
	"last_downloaded_at", "download_count", "id"}

// WriteCSV writes every candidate of in as CSV. Cells that a spreadsheet
// would evaluate as formulas are prefixed with an apostrophe.
func (s *Service) WriteCSV(ctx context.Context, in Input, w io.Writer) error {
	r, repos, err := s.plan(ctx, &in)
	if err != nil {
		return err
	}
	cw := csv.NewWriter(w)
	if err := cw.Write(CSVHeader); err != nil {
		return err
	}
	now := s.opts.Now().UTC()
	err = s.scan(ctx, r, repos, now, position{}, scanHooks{candidate: func(_ *core.Repository, c Candidate) error {
		last := ""
		if c.LastDownloadedAt != nil {
			last = c.LastDownloadedAt.Format(time.RFC3339)
		}
		return cw.Write([]string{safeCell(c.Repository), safeCell(c.Format), safeCell(c.Namespace), safeCell(c.Name),
			safeCell(c.Version), strconv.FormatInt(c.SizeBytes, 10), c.CreatedAt.Format(time.RFC3339), last,
			strconv.FormatInt(c.DownloadCount, 10), c.ID})
	}})
	if err != nil {
		return err
	}
	cw.Flush()
	return cw.Error()
}

func safeCell(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}
