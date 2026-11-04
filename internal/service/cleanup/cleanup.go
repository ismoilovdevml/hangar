// Package cleanup implements cleanup policies: criteria that select package
// versions (components) for deletion, attached to repositories, previewed
// before they are saved, and executed manually or on a cron schedule as the
// policy's task (see internal/tasks).
//
// A policy applies to the repositories attached to it (policy_repos) and to
// every repository whose configuration lists the policy by name
// (config.cleanup_policies), restricted to the policy's format. Group
// repositories store nothing and are never cleaned.
//
// Preview and execution share one selection (Criteria, evaluated per
// package over the versions in a repository), so a run deletes exactly what
// a preview at the same moment lists. Execution deletes through
// core.ComponentService.DeleteComponent, so every deletion is authorized
// (as the system principal), audited, published as component.deleted and
// releases blob references; blob compaction frees the bytes later.
package cleanup

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/tasks"
)

// DefaultPreviewTTL is how long a preview token allows saving its criteria.
const DefaultPreviewTTL = time.Hour

// Policy is a cleanup policy (schema CleanupPolicy).
type Policy struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Format      *string  `json:"format"`
	Criteria    Criteria `json:"criteria"`
	// Repositories are attached by the policy itself.
	Repositories []string `json:"repositories"`
	// EffectiveRepositories are the repositories the policy cleans: the
	// attached ones plus those whose configuration names the policy.
	EffectiveRepositories []string   `json:"effective_repositories"`
	Schedule              *string    `json:"schedule"`
	ScheduleText          string     `json:"schedule_text"`
	Enabled               bool       `json:"enabled"`
	TaskID                string     `json:"task_id"`
	NextRunAt             *time.Time `json:"next_run_at"`
	LastRun               *tasks.Run `json:"last_run"`
	CreatedAt             time.Time  `json:"created_at"`
	UpdatedAt             time.Time  `json:"updated_at"`
}

// Input creates, updates or previews a policy (schema CleanupPolicyInput).
type Input struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	Format       *string  `json:"format"`
	Criteria     Criteria `json:"criteria"`
	Repositories []string `json:"repositories"`
	Schedule     *string  `json:"schedule"`
	// Enabled defaults to true.
	Enabled *bool `json:"enabled"`
	// PreviewToken comes from a Preview of the same name, format, criteria
	// and repositories; it is required to save new or changed selection rules.
	PreviewToken string `json:"preview_token"`
}

// Options configure the service. Repos, Components and Tasks are required.
type Options struct {
	Repos      core.RepositoryService
	Components core.ComponentService
	Tasks      *tasks.Service
	Audit      core.AuditLog
	Logger     *slog.Logger
	// Now overrides the clock (tests).
	Now        func() time.Time
	PreviewTTL time.Duration
}

// Service manages and executes cleanup policies.
type Service struct {
	pool *pgxpool.Pool
	opts Options
	log  *slog.Logger
}

// New returns the service.
func New(pool *pgxpool.Pool, opts Options) *Service {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.PreviewTTL <= 0 {
		opts.PreviewTTL = DefaultPreviewTTL
	}
	return &Service{pool: pool, opts: opts, log: opts.Logger}
}

var (
	nameRE   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)
	formatRE = regexp.MustCompile(`^[a-z0-9]{1,32}$`)
)

// ErrPreviewRequired: saving new selection rules needs a matching preview token.
var ErrPreviewRequired = fmt.Errorf("preview the policy and pass its preview_token to save it: %w", core.ErrInvalid)

// ErrRunning: the policy has an active run.
var ErrRunning = fmt.Errorf("the policy is running; stop it first: %w", core.ErrAlreadyExists)

// ---- reads ----

const policySelect = `
SELECT p.id::text, p.name, p.description, p.format, p.criteria, p.schedule, p.enabled, p.created_at, p.updated_at,
	coalesce(t.id::text, ''), t.next_run_at,
	array(SELECT r.name FROM policy_repos pr JOIN repositories r ON r.id = pr.repo_id
		WHERE pr.policy_id = p.id ORDER BY r.name),
	array(SELECT r.name FROM repositories r
		WHERE r.type <> 'group' AND (p.format IS NULL OR r.format = p.format)
		  AND (r.id IN (SELECT repo_id FROM policy_repos WHERE policy_id = p.id)
		       OR r.config->'cleanup_policies' ? p.name)
		ORDER BY r.name)
FROM cleanup_policies p LEFT JOIN tasks t ON t.policy_id = p.id`

func scanPolicy(row pgx.Row) (*Policy, error) {
	var p Policy
	var crit []byte
	if err := row.Scan(&p.ID, &p.Name, &p.Description, &p.Format, &crit, &p.Schedule, &p.Enabled, &p.CreatedAt, &p.UpdatedAt,
		&p.TaskID, &p.NextRunAt, &p.Repositories, &p.EffectiveRepositories); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(crit, &p.Criteria); err != nil {
		return nil, fmt.Errorf("policy %q: decode criteria: %w", p.Name, err)
	}
	p.ScheduleText = tasks.ScheduleText(p.Schedule)
	p.CreatedAt, p.UpdatedAt = p.CreatedAt.UTC(), p.UpdatedAt.UTC()
	if p.NextRunAt != nil && p.Enabled && p.Schedule != nil {
		v := p.NextRunAt.UTC()
		p.NextRunAt = &v
	} else {
		p.NextRunAt = nil
	}
	return &p, nil
}

// Get returns a policy by name or id.
func (s *Service) Get(ctx context.Context, ref string) (*Policy, error) {
	p, err := s.get(ctx, s.pool, ref, false)
	if err != nil {
		return nil, err
	}
	s.fillLastRun(ctx, p)
	return p, nil
}

type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (s *Service) get(ctx context.Context, q querier, ref string, lock bool) (*Policy, error) {
	where := ` WHERE p.name = $1`
	if isUUID(ref) {
		where = ` WHERE p.id = $1::uuid`
	}
	sql := policySelect + where
	if lock {
		sql += ` FOR UPDATE OF p`
	}
	p, err := scanPolicy(q.QueryRow(ctx, sql, ref))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("cleanup policy %q: %w", ref, core.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get cleanup policy %q: %w", ref, err)
	}
	return p, nil
}

func (s *Service) fillLastRun(ctx context.Context, p *Policy) {
	if s.opts.Tasks == nil || p.TaskID == "" {
		return
	}
	t, err := s.opts.Tasks.Get(ctx, p.TaskID)
	if err != nil {
		s.log.Warn("cleanup: load policy task", "policy", p.Name, "error", err)
		return
	}
	p.LastRun = t.LastRun
}

// List returns policies by name, keyset-paginated (the cursor is the last name).
func (s *Service) List(ctx context.Context, cursor string, limit int) (core.Page[*Policy], error) {
	limit = core.ClampLimit(limit)
	after := ""
	if cursor != "" {
		b, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil || !nameRE.Match(b) {
			return core.Page[*Policy]{}, fmt.Errorf("cursor %q: %w", cursor, core.ErrInvalid)
		}
		after = string(b)
	}
	rows, err := s.pool.Query(ctx, policySelect+` WHERE p.name > $1 ORDER BY p.name LIMIT $2`, after, limit+1)
	if err != nil {
		return core.Page[*Policy]{}, fmt.Errorf("list cleanup policies: %w", err)
	}
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*Policy, error) { return scanPolicy(r) })
	if err != nil {
		return core.Page[*Policy]{}, fmt.Errorf("list cleanup policies: %w", err)
	}
	page := core.Page[*Policy]{Items: items}
	if len(items) > limit {
		page.Items = items[:limit]
		page.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(page.Items[limit-1].Name))
	}
	for _, p := range page.Items {
		s.fillLastRun(ctx, p)
	}
	return page, nil
}

// ---- validation and repository resolution ----

func (in *Input) normalize() {
	in.Name = strings.TrimSpace(in.Name)
	if in.Format != nil {
		f := strings.TrimSpace(*in.Format)
		in.Format = &f
		if f == "" {
			in.Format = nil
		}
	}
	if in.Schedule != nil && strings.TrimSpace(*in.Schedule) == "" {
		in.Schedule = nil
	} else if in.Schedule != nil {
		v := strings.TrimSpace(*in.Schedule)
		in.Schedule = &v
	}
	seen := map[string]bool{}
	repos := make([]string, 0, len(in.Repositories))
	for _, r := range in.Repositories {
		r = strings.TrimSpace(r)
		if r != "" && !seen[r] {
			seen[r] = true
			repos = append(repos, r)
		}
	}
	slices.Sort(repos)
	in.Repositories = repos
}

func (s *Service) validate(in *Input) (*rules, error) {
	in.normalize()
	if !nameRE.MatchString(in.Name) {
		return nil, fmt.Errorf("policy name %q must match %s: %w", in.Name, nameRE, core.ErrInvalid)
	}
	if in.Format != nil && !formatRE.MatchString(*in.Format) {
		return nil, fmt.Errorf("format %q: %w", *in.Format, core.ErrInvalid)
	}
	if len(in.Description) > 1000 {
		return nil, fmt.Errorf("description longer than 1000 characters: %w", core.ErrInvalid)
	}
	if err := tasks.ValidateSchedule(in.Schedule); err != nil {
		return nil, err
	}
	return in.Criteria.compile()
}

// attached resolves the explicitly attached repositories, checking that
// they exist, store content and match the format.
func (s *Service) attached(ctx context.Context, in *Input) ([]*core.Repository, error) {
	out := make([]*core.Repository, 0, len(in.Repositories))
	for _, name := range in.Repositories {
		r, err := s.opts.Repos.Get(ctx, name)
		if errors.Is(err, core.ErrNotFound) {
			return nil, fmt.Errorf("repository %q does not exist: %w", name, core.ErrInvalid)
		}
		if err != nil {
			return nil, err
		}
		if r.Type == core.RepoGroup {
			return nil, fmt.Errorf("repository %q is a group; attach its members: %w", name, core.ErrInvalid)
		}
		if in.Format != nil && r.Format != *in.Format {
			return nil, fmt.Errorf("repository %q is %s, the policy is for %s: %w", name, r.Format, *in.Format, core.ErrInvalid)
		}
		out = append(out, r)
	}
	return out, nil
}

// effective returns the attached repositories plus those whose config names
// the policy, by name.
func (s *Service) effective(ctx context.Context, in *Input) ([]*core.Repository, error) {
	repos, err := s.attached(ctx, in)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT name FROM repositories
WHERE type <> 'group' AND config->'cleanup_policies' ? $1 AND ($2::text IS NULL OR format = $2) ORDER BY name`, in.Name, in.Format)
	if err != nil {
		return nil, fmt.Errorf("find repositories naming policy %q: %w", in.Name, err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("find repositories naming policy %q: %w", in.Name, err)
	}
	for _, n := range names {
		if slices.ContainsFunc(repos, func(r *core.Repository) bool { return r.Name == n }) {
			continue
		}
		r, err := s.opts.Repos.Get(ctx, n)
		if errors.Is(err, core.ErrNotFound) {
			continue // deleted meanwhile
		}
		if err != nil {
			return nil, err
		}
		repos = append(repos, r)
	}
	slices.SortFunc(repos, func(a, b *core.Repository) int { return strings.Compare(a.Name, b.Name) })
	return repos, nil
}

// fingerprint identifies the selection rules of an input.
func fingerprint(in *Input) string {
	b, _ := json.Marshal(struct {
		Name     string   `json:"name"`
		Format   *string  `json:"format"`
		Criteria Criteria `json:"criteria"`
		Repos    []string `json:"repositories"`
	}{in.Name, in.Format, in.Criteria, in.Repositories})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *Service) checkPreview(ctx context.Context, q querier, in *Input) error {
	if in.PreviewToken == "" {
		return ErrPreviewRequired
	}
	var fp string
	err := q.QueryRow(ctx, `SELECT fingerprint FROM cleanup_previews WHERE token_hash = $1 AND expires_at > now()`,
		hashToken(in.PreviewToken)).Scan(&fp)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("preview_token is unknown or expired; preview again: %w", core.ErrInvalid)
	}
	if err != nil {
		return fmt.Errorf("check preview token: %w", err)
	}
	if fp != fingerprint(in) {
		return fmt.Errorf("preview_token was issued for different criteria or repositories; preview again: %w", core.ErrInvalid)
	}
	return nil
}

// ---- writes ----

// Create saves a new policy and its task. The caller authorizes; Create audits.
func (s *Service) Create(ctx context.Context, in Input) (*Policy, error) {
	if _, err := s.validate(&in); err != nil {
		return nil, err
	}
	repos, err := s.attached(ctx, &in)
	if err != nil {
		return nil, err
	}
	var id string
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := s.checkPreview(ctx, tx, &in); err != nil {
			return err
		}
		crit, _ := json.Marshal(in.Criteria)
		err := tx.QueryRow(ctx, `INSERT INTO cleanup_policies (name, description, format, criteria, schedule, enabled)
VALUES ($1, $2, $3, $4, $5, $6) RETURNING id::text`,
			in.Name, in.Description, in.Format, crit, in.Schedule, in.Enabled == nil || *in.Enabled).Scan(&id)
		if isUniqueViolation(err) {
			return fmt.Errorf("cleanup policy %q: %w", in.Name, core.ErrAlreadyExists)
		}
		if err != nil {
			return fmt.Errorf("create cleanup policy: %w", err)
		}
		return s.attach(ctx, tx, id, &in, repos)
	})
	if err != nil {
		return nil, err
	}
	s.audit(ctx, "cleanup.policy.create", in.Name, map[string]any{"criteria": in.Criteria, "repositories": in.Repositories,
		"format": in.Format, "schedule": in.Schedule})
	return s.Get(ctx, id)
}

func (s *Service) attach(ctx context.Context, tx pgx.Tx, id string, in *Input, repos []*core.Repository) error {
	if _, err := tx.Exec(ctx, `DELETE FROM policy_repos WHERE policy_id = $1`, id); err != nil {
		return fmt.Errorf("attach repositories: %w", err)
	}
	ids := make([]string, len(repos))
	for i, r := range repos {
		ids[i] = r.ID
	}
	if _, err := tx.Exec(ctx, `INSERT INTO policy_repos (policy_id, repo_id) SELECT $1, unnest($2::uuid[])`, id, ids); err != nil {
		return mapErr("attach repositories", err)
	}
	_, err := tasks.SyncPolicyTask(ctx, tx, tasks.PolicyTask{PolicyID: id, PolicyName: in.Name,
		Description: "Cleanup policy " + in.Name, Schedule: in.Schedule, Enabled: in.Enabled == nil || *in.Enabled}, s.opts.Now())
	return err
}

// Update replaces a policy. The name cannot change. A preview token is
// required when the format, criteria or repositories change. The caller
// authorizes; Update audits.
func (s *Service) Update(ctx context.Context, ref string, in Input) (*Policy, error) {
	cur, err := s.get(ctx, s.pool, ref, false)
	if err != nil {
		return nil, err
	}
	if in.Name == "" {
		in.Name = cur.Name
	}
	if in.Name != cur.Name {
		return nil, fmt.Errorf("a cleanup policy cannot be renamed (repositories reference it by name): %w", core.ErrInvalid)
	}
	if _, err := s.validate(&in); err != nil {
		return nil, err
	}
	repos, err := s.attached(ctx, &in)
	if err != nil {
		return nil, err
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		locked, err := s.get(ctx, tx, cur.ID, true)
		if err != nil {
			return err
		}
		before := Input{Name: locked.Name, Format: locked.Format, Criteria: locked.Criteria, Repositories: locked.Repositories}
		before.normalize()
		if fingerprint(&before) != fingerprint(&in) {
			if err := s.checkPreview(ctx, tx, &in); err != nil {
				return err
			}
		}
		crit, _ := json.Marshal(in.Criteria)
		if _, err := tx.Exec(ctx, `UPDATE cleanup_policies SET description = $2, format = $3, criteria = $4, schedule = $5,
	enabled = $6, updated_at = now() WHERE id = $1`, cur.ID, in.Description, in.Format, crit, in.Schedule,
			in.Enabled == nil || *in.Enabled); err != nil {
			return fmt.Errorf("update cleanup policy %q: %w", cur.Name, err)
		}
		return s.attach(ctx, tx, cur.ID, &in, repos)
	})
	if err != nil {
		return nil, err
	}
	s.audit(ctx, "cleanup.policy.update", in.Name, map[string]any{"criteria": in.Criteria, "repositories": in.Repositories,
		"format": in.Format, "schedule": in.Schedule, "enabled": in.Enabled == nil || *in.Enabled})
	return s.Get(ctx, cur.ID)
}

// Delete removes a policy with its task and run history. A running policy
// must be stopped first. The caller authorizes; Delete audits.
func (s *Service) Delete(ctx context.Context, ref string) error {
	cur, err := s.get(ctx, s.pool, ref, false)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM cleanup_policies p WHERE p.id = $1 AND NOT EXISTS (
	SELECT 1 FROM task_runs r JOIN tasks t ON t.id = r.task_id WHERE t.policy_id = p.id AND r.status IN ('queued', 'running'))`, cur.ID)
	if err != nil {
		return fmt.Errorf("delete cleanup policy %q: %w", cur.Name, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("cleanup policy %q: %w", cur.Name, ErrRunning)
	}
	s.audit(ctx, "cleanup.policy.delete", cur.Name, nil)
	return nil
}

// Run queues a run of the policy now (also when the policy is disabled,
// which only stops its schedule). The caller authorizes; the run is audited.
func (s *Service) Run(ctx context.Context, ref, triggeredBy string) (*tasks.Run, error) {
	p, err := s.get(ctx, s.pool, ref, false)
	if err != nil {
		return nil, err
	}
	if p.TaskID == "" {
		return nil, fmt.Errorf("cleanup policy %q has no task: %w", p.Name, core.ErrNotFound)
	}
	return s.opts.Tasks.Trigger(ctx, p.TaskID, triggeredBy)
}

// Stop stops the policy's active run.
func (s *Service) Stop(ctx context.Context, ref string) (*tasks.Run, error) {
	p, err := s.get(ctx, s.pool, ref, false)
	if err != nil {
		return nil, err
	}
	return s.opts.Tasks.Stop(ctx, p.TaskID)
}

// Runs lists the policy's runs, newest first.
func (s *Service) Runs(ctx context.Context, ref, cursor string, limit int) (core.Page[*tasks.Run], error) {
	p, err := s.get(ctx, s.pool, ref, false)
	if err != nil {
		return core.Page[*tasks.Run]{}, err
	}
	return s.opts.Tasks.Runs(ctx, p.TaskID, cursor, limit)
}

// PurgePreviews deletes expired preview tokens.
func (s *Service) PurgePreviews(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM cleanup_previews WHERE expires_at <= now()`)
	if err != nil {
		return 0, fmt.Errorf("purge cleanup previews: %w", err)
	}
	return tag.RowsAffected(), nil
}

func newToken() string {
	var b [24]byte
	_, _ = rand.Read(b[:])
	return "cpv_" + base64.RawURLEncoding.EncodeToString(b[:])
}

func (s *Service) audit(ctx context.Context, action, policy string, details map[string]any) {
	if s.opts.Audit == nil {
		return
	}
	if err := s.opts.Audit.Record(ctx, core.AuditEvent{Action: action, Target: "cleanup-policy:" + policy, Details: details}); err != nil {
		s.log.WarnContext(ctx, "audit record failed", "action", action, "error", err)
	}
}

func isUniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

func mapErr(what string, err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && (pg.Code == "23503" || pg.Code == "22P02") {
		return fmt.Errorf("%s: %s: %w", what, pg.Message, core.ErrInvalid)
	}
	return fmt.Errorf("%s: %w", what, err)
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
				return false
			}
		}
	}
	return true
}
