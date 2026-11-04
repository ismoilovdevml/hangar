package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"hash/crc32"
	"strings"

	"github.com/ismoilovdevml/hangar/internal/core"
)

const (
	base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	// tokenRandomLen base62 characters carry ~238 bits of entropy.
	tokenRandomLen = 40
	// tokenChecksumLen base62 characters encode the CRC-32 of the random part,
	// so scanners and the server can reject mistyped or fabricated tokens
	// without a database lookup.
	tokenChecksumLen = 6
	tokenLen         = len(core.TokenPrefix) + tokenRandomLen + tokenChecksumLen
	// tokenDisplayLen is the length of Token.Prefix ("hgr_1a2b").
	tokenDisplayLen = len(core.TokenPrefix) + 4
)

// newTokenSecret returns "hgr_" + 40 random base62 characters + 6-character checksum.
func newTokenSecret() (string, error) {
	random, err := randomBase62(tokenRandomLen)
	if err != nil {
		return "", err
	}
	return core.TokenPrefix + random + checksum62(random), nil
}

// wellFormedToken reports whether s looks like a secret Hangar issued.
func wellFormedToken(s string) bool {
	if len(s) != tokenLen || !strings.HasPrefix(s, core.TokenPrefix) {
		return false
	}
	body := s[len(core.TokenPrefix):]
	for i := 0; i < len(body); i++ {
		if strings.IndexByte(base62, body[i]) < 0 {
			return false
		}
	}
	random, sum := body[:tokenRandomLen], body[tokenRandomLen:]
	return checksum62(random) == sum
}

func checksum62(s string) string {
	v := crc32.ChecksumIEEE([]byte(s))
	out := make([]byte, tokenChecksumLen)
	for i := tokenChecksumLen - 1; i >= 0; i-- {
		out[i] = base62[v%62]
		v /= 62
	}
	return string(out)
}

// randomBase62 returns n uniformly random base62 characters (rejection sampling).
func randomBase62(n int) (string, error) {
	out := make([]byte, 0, n)
	buf := make([]byte, n+n/2)
	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		for _, b := range buf {
			if b < 248 { // 248 = 4*62: keeps the distribution uniform
				out = append(out, base62[b%62])
				if len(out) == n {
					break
				}
			}
		}
	}
	return string(out), nil
}

// randomURLSafe returns n random bytes, base64url encoded (session secrets, CSRF tokens).
func randomURLSafe(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// hashSecret is how token and session secrets are stored.
func hashSecret(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}
