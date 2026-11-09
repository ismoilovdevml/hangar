package auth

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// maxIDToken bounds the ID token kept with a session.
const maxIDToken = 16 << 10

const idTokenInfo = "hangar session id_token v1"

// sessionAEAD is the cipher sealing the ID token of the session with secret:
// its key is derived from the cookie secret, which is never stored, so the
// sealed token is useless to someone who can only read the database.
func sessionAEAD(secret string) (cipher.AEAD, error) {
	key, err := hkdf.Key(sha256.New, []byte(secret), nil, idTokenInfo, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// SetSessionIDToken keeps the OpenID Connect ID token a single sign-on
// session was started with, sealed with the session secret, for the
// id_token_hint of RP-initiated logout. An oversized token is not kept
// (logout then goes without the hint).
func (s *Service) SetSessionIDToken(ctx context.Context, secret, idToken string) error {
	if secret == "" || idToken == "" || len(idToken) > maxIDToken {
		return nil
	}
	aead, err := sessionAEAD(secret)
	if err != nil {
		return fmt.Errorf("session id token: %w", err)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("session id token: %w", err)
	}
	h := hashSecret(secret)
	sealed := aead.Seal(nonce, nonce, []byte(idToken), h)
	_, err = s.pool.Exec(ctx, `UPDATE sessions SET id_token = $2 WHERE hash = $1`, h, sealed)
	return dbErr(err, "session id token")
}

// SessionIDToken returns the ID token kept with the session of secret, ""
// when there is none (not a single sign-on session, or it cannot be opened).
func (s *Service) SessionIDToken(ctx context.Context, secret string) (string, error) {
	if secret == "" || len(secret) > 128 {
		return "", nil
	}
	h := hashSecret(secret)
	var sealed []byte
	err := s.pool.QueryRow(ctx, `SELECT id_token FROM sessions WHERE hash = $1`, h).Scan(&sealed)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && len(sealed) == 0) {
		return "", nil
	}
	if err != nil {
		return "", dbErr(err, "session id token")
	}
	aead, err := sessionAEAD(secret)
	if err != nil {
		return "", fmt.Errorf("session id token: %w", err)
	}
	ns := aead.NonceSize()
	if len(sealed) < ns {
		return "", nil
	}
	plain, err := aead.Open(nil, sealed[:ns], sealed[ns:], h)
	if err != nil {
		return "", nil //nolint:nilerr // a token that does not open is as good as none
	}
	return string(plain), nil
}
