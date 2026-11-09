package signing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/secrets"
)

// EventKeyChanged is published when a key is rotated or imported; every
// node drops its cached copy. Subject is the key name.
const EventKeyChanged = "signing.key_changed"

// Key selection, the format setting signing_key of a repository.
const (
	// KeyRepository: the repository has its own key (default).
	KeyRepository = "repository"
	// KeyShared: the repository signs with the server-wide shared key.
	KeyShared = "shared"
)

// cacheTTL bounds how long a node keeps using a key another node replaced
// when the change event was lost.
const cacheTTL = time.Minute

// Signer gives formats the key that signs a repository's metadata.
type Signer interface {
	// KeyFor returns repo's signing key, generating and storing it on first use.
	KeyFor(ctx context.Context, repo *core.Repository) (*Key, error)
}

// Sealer encrypts private keys at rest (*secrets.Keyring).
type Sealer interface {
	SealString(s string) (string, error)
	OpenString(sealed string) (string, error)
}

// Options configure a Service.
type Options struct {
	Store Store
	// Secrets seals private keys; nil means no security.secret_key is
	// configured and keys can be neither created nor opened.
	Secrets Sealer
	// Algorithm of generated keys; empty means DefaultAlgorithm.
	Algorithm string
	// Locks serialises key creation across nodes (nil: this process only).
	Locks core.Locker
	// Events carries EventKeyChanged between nodes (optional).
	Events core.EventBus
	// Audit records rotations and imports (optional).
	Audit core.AuditLog
	// AuthZ and Repos serve the admin API (RegisterRoutes).
	AuthZ core.AuthZ
	Repos core.RepositoryService
	// BaseURL is used for the e-mail part of generated user ids.
	BaseURL string
	// Formats lists the formats whose repositories have signing keys.
	Formats []string
	Logger  *slog.Logger
}

// Service manages signing keys.
type Service struct {
	opts   Options
	log    *slog.Logger
	flight singleflight.Group

	mu    sync.Mutex
	cache map[string]cachedKey
}

type cachedKey struct {
	key *Key
	rec *Record
	at  time.Time
}

var _ Signer = (*Service)(nil)

// New returns a Service.
func New(opts Options) *Service {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if opts.Algorithm == "" {
		opts.Algorithm = DefaultAlgorithm
	}
	return &Service{opts: opts, log: log, cache: map[string]cachedKey{}}
}

// Watch drops cached keys when another node changes them.
func (s *Service) Watch(ctx context.Context, bus core.EventBus) (core.Subscription, error) {
	return bus.Subscribe(ctx, EventKeyChanged, func(_ context.Context, e core.Event) {
		s.forget(e.Subject)
	})
}

func (s *Service) forget(name string) {
	s.mu.Lock()
	delete(s.cache, name)
	s.mu.Unlock()
}

// SelectedKey returns the signing_key setting of repo: KeyRepository or KeyShared.
func SelectedKey(repo *core.Repository) string {
	if v, ok := repo.Config.Format["signing_key"].(string); ok && v == KeyShared {
		return KeyShared
	}
	return KeyRepository
}

// ValidateSetting checks the signing_key format setting.
func ValidateSetting(repo *core.Repository) error {
	v, ok := repo.Config.Format["signing_key"]
	if !ok {
		return nil
	}
	if s, _ := v.(string); s != KeyRepository && s != KeyShared {
		return fmt.Errorf("repository %q: signing_key %v: want %q or %q: %w", repo.Name, v, KeyRepository, KeyShared, core.ErrInvalid)
	}
	return nil
}

// KeyName is the store name of the key repo signs with.
func KeyName(repo *core.Repository) string {
	if SelectedKey(repo) == KeyShared {
		return SharedKeyName
	}
	return "repo:" + repo.ID
}

func (s *Service) host() string {
	if u, err := url.Parse(s.opts.BaseURL); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	return "localhost"
}

func (s *Service) userID(repo *core.Repository) string {
	if SelectedKey(repo) == KeyShared {
		return "Hangar repository signing key <signing@" + s.host() + ">"
	}
	return "Hangar " + repo.Name + " signing key <" + repo.Name + "@" + s.host() + ">"
}

// KeyFor implements Signer.
func (s *Service) KeyFor(ctx context.Context, repo *core.Repository) (*Key, error) {
	k, _, err := s.load(ctx, repo)
	return k, err
}

func (s *Service) load(ctx context.Context, repo *core.Repository) (*Key, *Record, error) {
	name := KeyName(repo)
	s.mu.Lock()
	c, ok := s.cache[name]
	s.mu.Unlock()
	if ok && time.Since(c.at) < cacheTTL {
		return c.key, c.rec, nil
	}
	v, err, _ := s.flight.Do(name, func() (any, error) {
		return s.loadOrCreate(context.WithoutCancel(ctx), repo, name)
	})
	if err != nil {
		return nil, nil, err
	}
	c = v.(cachedKey)
	return c.key, c.rec, nil
}

func (s *Service) loadOrCreate(ctx context.Context, repo *core.Repository, name string) (cachedKey, error) {
	rec, err := s.opts.Store.Get(ctx, name)
	if errors.Is(err, core.ErrNotFound) {
		rec, err = s.create(ctx, repo, name)
	}
	if err != nil {
		return cachedKey{}, err
	}
	return s.open(name, rec)
}

func (s *Service) open(name string, rec *Record) (cachedKey, error) {
	if s.opts.Secrets == nil {
		return cachedKey{}, fmt.Errorf("repository signing keys: %w", secrets.ErrNoKey)
	}
	armored, err := s.opts.Secrets.OpenString(rec.PrivateKey)
	if err != nil {
		return cachedKey{}, fmt.Errorf("open signing key %q: %w", name, err)
	}
	k, err := ParsePrivate([]byte(armored), "")
	if err != nil {
		return cachedKey{}, fmt.Errorf("signing key %q: %w", name, err)
	}
	c := cachedKey{key: k, rec: rec, at: time.Now()}
	s.mu.Lock()
	s.cache[name] = c
	s.mu.Unlock()
	return c, nil
}

// create generates and stores the key name, unless another node does so first.
func (s *Service) create(ctx context.Context, repo *core.Repository, name string) (*Record, error) {
	if s.opts.Secrets == nil {
		return nil, fmt.Errorf("repository signing keys: %w", secrets.ErrNoKey)
	}
	if s.opts.Locks != nil {
		unlock, err := s.opts.Locks.Lock(ctx, "signing-key:"+name)
		if err != nil {
			return nil, err
		}
		defer unlock()
		if rec, err := s.opts.Store.Get(ctx, name); err == nil {
			return rec, nil
		}
	}
	started := time.Now()
	k, err := Generate(s.opts.Algorithm, s.userID(repo))
	if err != nil {
		return nil, err
	}
	rec, err := s.record(repo, name, k)
	if err != nil {
		return nil, err
	}
	if err := s.opts.Store.Create(ctx, rec); err != nil {
		if errors.Is(err, core.ErrAlreadyExists) {
			return s.opts.Store.Get(ctx, name)
		}
		return nil, err
	}
	s.log.InfoContext(ctx, "generated repository signing key", "key", name, "repository", repo.Name,
		"algorithm", k.Algorithm(), "fingerprint", k.Fingerprint(), "took", time.Since(started).Round(time.Millisecond))
	return s.opts.Store.Get(ctx, name)
}

func (s *Service) record(repo *core.Repository, name string, k *Key) (*Record, error) {
	pub, err := k.PublicArmored()
	if err != nil {
		return nil, err
	}
	priv, err := k.PrivateArmored()
	if err != nil {
		return nil, err
	}
	sealed, err := s.opts.Secrets.SealString(string(priv))
	if err != nil {
		return nil, fmt.Errorf("seal signing key: %w", err)
	}
	rec := &Record{Name: name, Algorithm: k.Algorithm(), Fingerprint: k.Fingerprint(), UserID: k.UserID(),
		PublicKey: string(pub), PrivateKey: sealed}
	if name != SharedKeyName {
		rec.RepoID = repo.ID
	}
	return rec, nil
}

// Info describes a repository's signing key.
type Info struct {
	Repository  string    `json:"repository"`
	Key         string    `json:"key"` // KeyRepository or KeyShared
	Algorithm   string    `json:"algorithm"`
	Fingerprint string    `json:"fingerprint"`
	KeyID       string    `json:"key_id"`
	UserID      string    `json:"user_id"`
	CreatedAt   time.Time `json:"created_at"`
	PublicKey   string    `json:"public_key"`
}

func info(repo *core.Repository, k *Key, rec *Record) *Info {
	return &Info{Repository: repo.Name, Key: SelectedKey(repo), Algorithm: rec.Algorithm, Fingerprint: rec.Fingerprint,
		KeyID: k.KeyID(), UserID: rec.UserID, CreatedAt: k.CreatedAt(), PublicKey: rec.PublicKey}
}

// Info returns repo's key, generating it on first use.
func (s *Service) Info(ctx context.Context, repo *core.Repository) (*Info, error) {
	k, rec, err := s.load(ctx, repo)
	if err != nil {
		return nil, err
	}
	return info(repo, k, rec), nil
}

// Rotate replaces repo's key with a newly generated one. Clients must
// fetch the new public key; metadata is signed with the new key at once.
func (s *Service) Rotate(ctx context.Context, repo *core.Repository, algorithm string) (*Info, error) {
	if algorithm == "" {
		algorithm = s.opts.Algorithm
	}
	k, err := Generate(algorithm, s.userID(repo))
	if err != nil {
		return nil, err
	}
	return s.replace(ctx, repo, k, "signing_key.rotate")
}

// Import replaces repo's key with an existing private key (ASCII-armored or
// binary, optionally passphrase-protected).
func (s *Service) Import(ctx context.Context, repo *core.Repository, privateKey []byte, passphrase string) (*Info, error) {
	k, err := ParsePrivate(privateKey, passphrase)
	if err != nil {
		return nil, err
	}
	return s.replace(ctx, repo, k, "signing_key.import")
}

func (s *Service) replace(ctx context.Context, repo *core.Repository, k *Key, action string) (*Info, error) {
	if s.opts.Secrets == nil {
		return nil, fmt.Errorf("repository signing keys: %w", secrets.ErrNoKey)
	}
	name := KeyName(repo)
	var previous string
	if old, err := s.opts.Store.Get(ctx, name); err == nil {
		previous = old.Fingerprint
	}
	rec, err := s.record(repo, name, k)
	if err != nil {
		return nil, err
	}
	if err := s.opts.Store.Replace(ctx, rec); err != nil {
		return nil, err
	}
	s.forget(name)
	if s.opts.Events != nil {
		if err := s.opts.Events.Publish(ctx, core.Event{Type: EventKeyChanged, Subject: name, Repository: repo.Name}); err != nil {
			s.log.WarnContext(ctx, "publish signing key change", "key", name, "err", err)
		}
	}
	if s.opts.Audit != nil {
		target := "repo:" + repo.Name
		if name == SharedKeyName {
			target = "signing_key:shared"
		}
		_ = s.opts.Audit.Record(ctx, core.AuditEvent{Action: action, Target: target, Details: map[string]any{
			"fingerprint": k.Fingerprint(), "previous_fingerprint": previous, "algorithm": k.Algorithm(), "key": SelectedKey(repo),
		}})
	}
	s.log.InfoContext(ctx, "replaced repository signing key", "action", action, "key", name, "fingerprint", k.Fingerprint())
	stored, err := s.opts.Store.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	c, err := s.open(name, stored)
	if err != nil {
		return nil, err
	}
	return info(repo, c.key, c.rec), nil
}

// SetServices supplies what the server wires after the formats: the
// repository service and authorizer of the admin API and the cluster lock.
// Call it before serving requests.
func (s *Service) SetServices(repos core.RepositoryService, authz core.AuthZ, locks core.Locker) {
	s.opts.Repos, s.opts.AuthZ, s.opts.Locks = repos, authz, locks
}
