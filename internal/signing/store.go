package signing

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// SharedKeyName names the server-wide key.
const SharedKeyName = "shared"

// Record is a stored key. PrivateKey is sealed.
type Record struct {
	Name        string
	RepoID      string // "" for the shared key
	Algorithm   string
	Fingerprint string
	UserID      string
	PublicKey   string // ASCII-armored
	PrivateKey  string // sealed ASCII-armored private key
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Store persists keys.
type Store interface {
	// Get returns the key named name; core.ErrNotFound if absent.
	Get(ctx context.Context, name string) (*Record, error)
	// Create inserts rec; core.ErrAlreadyExists if the name is taken.
	Create(ctx context.Context, rec *Record) error
	// Replace overwrites the key named rec.Name (rotation, import), creating
	// it when absent.
	Replace(ctx context.Context, rec *Record) error
}

// PGStore keeps keys in the signing_keys table.
type PGStore struct{ pool *pgxpool.Pool }

// NewPGStore returns a store on pool.
func NewPGStore(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Get implements Store.
func (s *PGStore) Get(ctx context.Context, name string) (*Record, error) {
	var r Record
	var repoID *string
	err := s.pool.QueryRow(ctx, `SELECT name, repo_id::text, algorithm, fingerprint, user_id, public_key, private_key, created_at, updated_at
		FROM signing_keys WHERE name = $1`, name).
		Scan(&r.Name, &repoID, &r.Algorithm, &r.Fingerprint, &r.UserID, &r.PublicKey, &r.PrivateKey, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("signing key %q: %w", name, core.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("signing key %q: %w", name, err)
	}
	if repoID != nil {
		r.RepoID = *repoID
	}
	return &r, nil
}

// Create implements Store.
func (s *PGStore) Create(ctx context.Context, rec *Record) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO signing_keys (name, repo_id, algorithm, fingerprint, user_id, public_key, private_key)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		rec.Name, nullable(rec.RepoID), rec.Algorithm, rec.Fingerprint, rec.UserID, rec.PublicKey, rec.PrivateKey)
	var pe *pgconn.PgError
	switch {
	case errors.As(err, &pe) && pe.Code == "23505":
		return fmt.Errorf("signing key %q: %w", rec.Name, core.ErrAlreadyExists)
	case errors.As(err, &pe) && pe.Code == "23503":
		return fmt.Errorf("signing key %q: repository is gone: %w", rec.Name, core.ErrNotFound)
	case err != nil:
		return fmt.Errorf("store signing key %q: %w", rec.Name, err)
	}
	return nil
}

// Replace implements Store.
func (s *PGStore) Replace(ctx context.Context, rec *Record) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO signing_keys (name, repo_id, algorithm, fingerprint, user_id, public_key, private_key)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (name) DO UPDATE SET algorithm = excluded.algorithm, fingerprint = excluded.fingerprint,
			user_id = excluded.user_id, public_key = excluded.public_key, private_key = excluded.private_key,
			created_at = now(), updated_at = now()`,
		rec.Name, nullable(rec.RepoID), rec.Algorithm, rec.Fingerprint, rec.UserID, rec.PublicKey, rec.PrivateKey)
	if err != nil {
		return fmt.Errorf("store signing key %q: %w", rec.Name, err)
	}
	return nil
}

// MemStore keeps keys in memory, for tests and single-process tools.
type MemStore struct {
	mu   sync.Mutex
	recs map[string]Record
}

// NewMemStore returns an empty in-memory store.
func NewMemStore() *MemStore { return &MemStore{recs: map[string]Record{}} }

// Get implements Store.
func (m *MemStore) Get(_ context.Context, name string) (*Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.recs[name]
	if !ok {
		return nil, fmt.Errorf("signing key %q: %w", name, core.ErrNotFound)
	}
	return &r, nil
}

// Create implements Store.
func (m *MemStore) Create(_ context.Context, rec *Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.recs[rec.Name]; ok {
		return fmt.Errorf("signing key %q: %w", rec.Name, core.ErrAlreadyExists)
	}
	r := *rec
	r.CreatedAt, r.UpdatedAt = time.Now().UTC(), time.Now().UTC()
	m.recs[rec.Name] = r
	return nil
}

// Replace implements Store.
func (m *MemStore) Replace(_ context.Context, rec *Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := *rec
	r.CreatedAt, r.UpdatedAt = time.Now().UTC(), time.Now().UTC()
	m.recs[rec.Name] = r
	return nil
}
