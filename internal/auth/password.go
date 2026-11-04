package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// errBadHash reports a stored hash that cannot be parsed.
var errBadHash = errors.New("malformed password hash")

// Hasher hashes and verifies passwords with argon2id, encoded in the PHC
// string format: $argon2id$v=19$m=65536,t=3,p=2$<salt>$<key> (unpadded base64).
// A semaphore bounds concurrent computations.
type Hasher struct {
	params Argon2Params
	sem    chan struct{}
}

// NewHasher returns a hasher using params for new hashes, allowing at most
// concurrency simultaneous computations.
func NewHasher(params Argon2Params, concurrency int) *Hasher {
	if concurrency < 1 {
		concurrency = 1
	}
	return &Hasher{params: params, sem: make(chan struct{}, concurrency)}
}

func (h *Hasher) acquire(ctx context.Context) error {
	select {
	case h.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *Hasher) release() { <-h.sem }

// Hash returns the PHC-encoded argon2id hash of password with a fresh salt.
func (h *Hasher) Hash(ctx context.Context, password string) (string, error) {
	salt := make([]byte, h.params.SaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("password salt: %w", err)
	}
	if err := h.acquire(ctx); err != nil {
		return "", err
	}
	defer h.release()
	p := h.params
	key := argon2.IDKey([]byte(password), salt, p.Iterations, p.Memory, p.Parallelism, p.KeyLength)
	return encodeHash(p, salt, key), nil
}

// Verify reports whether password matches encoded, and whether encoded was
// made with parameters other than the hasher's (the caller should re-hash).
// The comparison is constant-time.
func (h *Hasher) Verify(ctx context.Context, encoded, password string) (ok, rehash bool, err error) {
	p, salt, key, err := decodeHash(encoded)
	if err != nil {
		return false, false, err
	}
	if err := h.acquire(ctx); err != nil {
		return false, false, err
	}
	defer h.release()
	got := argon2.IDKey([]byte(password), salt, p.Iterations, p.Memory, p.Parallelism, uint32(len(key))) //nolint:gosec // key length comes from a parsed hash, bounded by decodeHash
	if subtle.ConstantTimeCompare(got, key) != 1 {
		return false, false, nil
	}
	want := h.params
	rehash = p.Memory != want.Memory || p.Iterations != want.Iterations || p.Parallelism != want.Parallelism ||
		uint32(len(salt)) != want.SaltLength || uint32(len(key)) != want.KeyLength //nolint:gosec // bounded by decodeHash
	return true, rehash, nil
}

func encodeHash(p Argon2Params, salt, key []byte) string {
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.Memory, p.Iterations, p.Parallelism, b64.EncodeToString(salt), b64.EncodeToString(key))
}

// decodeHash parses a PHC argon2id string. Parameters are bounded so a
// tampered row cannot make verification allocate unbounded memory.
func decodeHash(s string) (Argon2Params, []byte, []byte, error) {
	parts := strings.Split(s, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return Argon2Params{}, nil, nil, errBadHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return Argon2Params{}, nil, nil, errBadHash
	}
	var p Argon2Params
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.Memory, &p.Iterations, &p.Parallelism); err != nil {
		return Argon2Params{}, nil, nil, errBadHash
	}
	if p.Memory < 8 || p.Memory > 4*1024*1024 || p.Iterations < 1 || p.Iterations > 100 || p.Parallelism < 1 {
		return Argon2Params{}, nil, nil, errBadHash
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) < 8 || len(salt) > 64 {
		return Argon2Params{}, nil, nil, errBadHash
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(key) < 16 || len(key) > 128 {
		return Argon2Params{}, nil, nil, errBadHash
	}
	p.SaltLength, p.KeyLength = uint32(len(salt)), uint32(len(key)) //nolint:gosec // bounded above
	return p, salt, key, nil
}

// PasswordPolicy constrains local passwords. Length matters more than
// composition (NIST SP 800-63B), so MinClasses defaults to 0.
type PasswordPolicy struct {
	MinLength int `yaml:"min_length"` // characters (runes)
	MaxLength int `yaml:"max_length"`
	// MinClasses requires characters from at least this many of: lower case,
	// upper case, digits, other.
	MinClasses int `yaml:"min_classes"`
	// AllowUsername permits passwords that contain the username.
	AllowUsername bool `yaml:"allow_username"`
}

// DefaultPasswordPolicy: 12 to 256 characters, not containing the username.
var DefaultPasswordPolicy = PasswordPolicy{MinLength: 12, MaxLength: 256}

// Check returns an ErrInvalid error describing why password is unacceptable for username.
func (p PasswordPolicy) Check(username, password string) error {
	n := utf8.RuneCountInString(password)
	switch {
	case !utf8.ValidString(password):
		return fmt.Errorf("password is not valid UTF-8: %w", core.ErrInvalid)
	case n < p.MinLength:
		return fmt.Errorf("password must be at least %d characters: %w", p.MinLength, core.ErrInvalid)
	case p.MaxLength > 0 && n > p.MaxLength:
		return fmt.Errorf("password must be at most %d characters: %w", p.MaxLength, core.ErrInvalid)
	case strings.TrimSpace(password) == "":
		return fmt.Errorf("password must not be blank: %w", core.ErrInvalid)
	case !p.AllowUsername && username != "" && strings.Contains(strings.ToLower(password), strings.ToLower(username)):
		return fmt.Errorf("password must not contain the username: %w", core.ErrInvalid)
	}
	if p.MinClasses > 0 {
		var lower, upper, digit, other bool
		for _, r := range password {
			switch {
			case unicode.IsLower(r):
				lower = true
			case unicode.IsUpper(r):
				upper = true
			case unicode.IsDigit(r):
				digit = true
			default:
				other = true
			}
		}
		classes := 0
		for _, b := range []bool{lower, upper, digit, other} {
			if b {
				classes++
			}
		}
		if classes < p.MinClasses {
			return fmt.Errorf("password must mix at least %d of: lower case, upper case, digits, symbols: %w", p.MinClasses, core.ErrInvalid)
		}
	}
	return nil
}
