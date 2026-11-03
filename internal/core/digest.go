package core

import (
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"hash"
	"strings"
	"time"
)

// Digest identifies blob content: "<algorithm>:<lowercase hex>", e.g.
// "sha256:2cf24dba...". It uses the same syntax as OCI descriptors, so OCI
// digests can be stored as-is. Blob stores only produce sha256 digests.
type Digest string

// AlgorithmSHA256 is the only algorithm blob stores address content by.
const AlgorithmSHA256 = "sha256"

// NewSHA256Digest builds a digest from a raw sha256 sum.
func NewSHA256Digest(sum []byte) Digest {
	return Digest(AlgorithmSHA256 + ":" + hex.EncodeToString(sum))
}

// SHA256Of returns the digest of b. Intended for tests and small metadata documents.
func SHA256Of(b []byte) Digest {
	s := sha256.Sum256(b)
	return NewSHA256Digest(s[:])
}

// ParseDigest validates s and returns it as a Digest.
func ParseDigest(s string) (Digest, error) {
	d := Digest(s)
	if err := d.Validate(); err != nil {
		return "", err
	}
	return d, nil
}

// Algorithm returns the part before the colon ("sha256").
func (d Digest) Algorithm() string {
	a, _, _ := strings.Cut(string(d), ":")
	return a
}

// Hex returns the encoded hash after the colon.
func (d Digest) Hex() string {
	_, h, _ := strings.Cut(string(d), ":")
	return h
}

// String implements fmt.Stringer.
func (d Digest) String() string { return string(d) }

// Validate checks the syntax. Only sha256 (64 hex chars) and sha512 (128 hex
// chars) are accepted, lowercase. A valid digest is safe to use in file paths.
func (d Digest) Validate() error {
	algo, h, ok := strings.Cut(string(d), ":")
	if !ok {
		return fmt.Errorf("digest %q: missing algorithm: %w", string(d), ErrInvalid)
	}
	var want int
	switch algo {
	case AlgorithmSHA256:
		want = 64
	case "sha512":
		want = 128
	default:
		return fmt.Errorf("digest %q: unsupported algorithm: %w", string(d), ErrInvalid)
	}
	if len(h) != want {
		return fmt.Errorf("digest %q: want %d hex characters: %w", string(d), want, ErrInvalid)
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return fmt.Errorf("digest %q: not lowercase hex: %w", string(d), ErrInvalid)
		}
	}
	return nil
}

// NewDigester returns a hash for the digest's algorithm, so callers can verify
// client-declared digests while streaming. It returns nil for unknown algorithms.
func (d Digest) NewDigester() hash.Hash {
	switch d.Algorithm() {
	case AlgorithmSHA256:
		return sha256.New()
	case "sha512":
		return sha512.New()
	}
	return nil
}

// BlobInfo describes a stored blob.
type BlobInfo struct {
	Digest    Digest    `json:"digest"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"created_at"`
}
