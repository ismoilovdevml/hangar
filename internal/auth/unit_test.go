package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

var testArgon = Argon2Params{Memory: 1024, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32}

func TestHasherRoundTrip(t *testing.T) {
	ctx := context.Background()
	h := NewHasher(testArgon, 2)
	enc, err := h.Hash(ctx, "s3cret-passphrase")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(enc, "$argon2id$v=19$m=1024,t=1,p=1$") {
		t.Fatalf("encoding = %s", enc)
	}
	enc2, _ := h.Hash(ctx, "s3cret-passphrase")
	if enc == enc2 {
		t.Error("two hashes of the same password are equal: salt not random")
	}
	for _, tc := range []struct {
		pw     string
		wantOK bool
	}{{"s3cret-passphrase", true}, {"s3cret-passphrasE", false}, {"", false}} {
		ok, rehash, err := h.Verify(ctx, enc, tc.pw)
		if err != nil || ok != tc.wantOK || rehash {
			t.Errorf("Verify(%q) = %v, %v, %v; want %v, false, nil", tc.pw, ok, rehash, err, tc.wantOK)
		}
	}
	// Changed parameters: old hashes still verify and ask for a rehash.
	stronger := testArgon
	stronger.Memory, stronger.Iterations = 2048, 2
	ok, rehash, err := NewHasher(stronger, 1).Verify(ctx, enc, "s3cret-passphrase")
	if err != nil || !ok || !rehash {
		t.Errorf("verify with new params = %v, %v, %v; want true, true, nil", ok, rehash, err)
	}
}

func TestDecodeHashRejectsMalformed(t *testing.T) {
	good := "$argon2id$v=19$m=1024,t=1,p=1$c2FsdHNhbHRzYWx0c2FsdA$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNoaGFzaGhhc2g"
	if _, _, _, err := decodeHash(good); err != nil {
		t.Fatalf("good hash rejected: %v", err)
	}
	for name, s := range map[string]string{
		"empty":       "",
		"bcrypt":      "$2a$10$abcdefghijklmnopqrstuv",
		"argon2i":     strings.Replace(good, "argon2id", "argon2i", 1),
		"version":     strings.Replace(good, "v=19", "v=16", 1),
		"huge memory": strings.Replace(good, "m=1024", "m=99999999", 1),
		"zero t":      strings.Replace(good, "t=1", "t=0", 1),
		"bad salt":    strings.Replace(good, "c2FsdHNhbHRzYWx0c2FsdA", "!!", 1),
		"short key":   strings.Replace(good, "aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNoaGFzaGhhc2g", "aGFzaA", 1),
		"parts":       good + "$x",
	} {
		if _, _, _, err := decodeHash(s); !errors.Is(err, errBadHash) {
			t.Errorf("%s: err = %v, want errBadHash", name, err)
		}
	}
}

func TestPasswordPolicy(t *testing.T) {
	p := DefaultPasswordPolicy
	strict := PasswordPolicy{MinLength: 8, MaxLength: 64, MinClasses: 3}
	for _, tc := range []struct {
		name   string
		policy PasswordPolicy
		user   string
		pw     string
		ok     bool
	}{
		{"long enough", p, "alice", "a long passphrase", true},
		{"too short", p, "alice", "short", false},
		{"unicode counts runes", p, "alice", "пароль-пароль", true},
		{"contains username", p, "alice", "my-name-is-ALICE-ok", false},
		{"username allowed", PasswordPolicy{MinLength: 12, MaxLength: 256, AllowUsername: true}, "alice", "my-name-is-alice", true},
		{"too long", p, "alice", strings.Repeat("x", 257), false},
		{"blank", p, "alice", strings.Repeat(" ", 20), false},
		{"classes ok", strict, "bob", "abcDEF12", true},
		{"classes missing", strict, "bob", "abcdefgh1", false},
		{"invalid utf8", p, "alice", "abcdefghijkl\xff", false},
	} {
		err := tc.policy.Check(tc.user, tc.pw)
		if (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok=%v", tc.name, err, tc.ok)
		}
		if err != nil && !errors.Is(err, core.ErrInvalid) {
			t.Errorf("%s: err %v does not wrap ErrInvalid", tc.name, err)
		}
	}
}

func TestTokenSecretFormat(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		s, err := newTokenSecret()
		if err != nil {
			t.Fatal(err)
		}
		if len(s) != tokenLen || !strings.HasPrefix(s, core.TokenPrefix) || !wellFormedToken(s) {
			t.Fatalf("bad secret %q", s)
		}
		if seen[s] {
			t.Fatal("duplicate secret")
		}
		seen[s] = true
	}
	s, _ := newTokenSecret()
	flipped := []byte(s)
	if flipped[10] == 'a' {
		flipped[10] = 'b'
	} else {
		flipped[10] = 'a'
	}
	for name, bad := range map[string]string{
		"typo":      string(flipped),
		"truncated": s[:len(s)-1],
		"no prefix": "xyz_" + s[4:],
		"bad char":  s[:20] + "-" + s[21:],
		"empty":     "",
	} {
		if wellFormedToken(bad) {
			t.Errorf("%s: %q accepted", name, bad)
		}
	}
}

func TestRateLimiter(t *testing.T) {
	now := time.Unix(1000, 0)
	clock := func() time.Time { return now }
	l := NewRateLimiter(RateLimit{PerMinute: 6, Burst: 3}, clock) // one token per 10s
	for i := range 3 {
		if ok, _ := l.Allow("1.2.3.4"); !ok {
			t.Fatalf("attempt %d denied within burst", i+1)
		}
	}
	ok, wait := l.Allow("1.2.3.4")
	if ok || wait <= 0 || wait > 10*time.Second {
		t.Fatalf("4th attempt = %v, wait %v; want denied with wait <= 10s", ok, wait)
	}
	if ok, _ := l.Allow("5.6.7.8"); !ok {
		t.Fatal("other IP denied")
	}
	now = now.Add(10 * time.Second)
	if ok, _ := l.Allow("1.2.3.4"); !ok {
		t.Fatal("not refilled after 10s")
	}
	if ok, _ := (*RateLimiter)(nil).Allow("x"); !ok {
		t.Fatal("nil limiter must allow")
	}
	if NewRateLimiter(RateLimit{PerMinute: -1}, nil) != nil {
		t.Fatal("disabled limiter should be nil")
	}
}

func TestTTLCache(t *testing.T) {
	now := time.Unix(0, 0)
	c := newTTLCache[string, int](time.Minute, 2, func() time.Time { return now })
	c.set("a", 1)
	if v, ok := c.get("a"); !ok || v != 1 {
		t.Fatal("miss after set")
	}
	now = now.Add(time.Minute)
	if _, ok := c.get("a"); ok {
		t.Fatal("hit after expiry")
	}
	c.set("a", 1)
	c.set("b", 2)
	c.set("c", 3) // full: nothing expired, so everything is dropped first
	if _, ok := c.get("a"); ok {
		t.Fatal("bound not enforced")
	}
	if _, ok := c.get("c"); !ok {
		t.Fatal("newest entry missing")
	}
	c.clear()
	if _, ok := c.get("c"); ok {
		t.Fatal("hit after clear")
	}
}

func TestConfigDefaultsAndValidate(t *testing.T) {
	c := Config{}.WithDefaults()
	if err := c.Validate(); err != nil {
		t.Fatalf("defaults invalid: %v", err)
	}
	if c.Session.CookieName != "hangar_session" || c.Lockout.MaxAttempts != 5 || c.Argon2 != DefaultArgon2 {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	bad := c
	bad.Password.MinLength = 4
	if err := bad.Validate(); !errors.Is(err, core.ErrInvalid) {
		t.Errorf("weak min length accepted: %v", err)
	}
	bad = c
	bad.Argon2.Memory = 64
	if err := bad.Validate(); !errors.Is(err, core.ErrInvalid) {
		t.Errorf("tiny argon2 memory accepted: %v", err)
	}
}
