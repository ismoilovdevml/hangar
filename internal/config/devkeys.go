package config

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

// The development keys of deploy/docker-compose.yml. They are public, so a
// server using them can have its registry tokens forged and its stored
// secrets opened by anyone; only a server with server.dev_mode may start
// with them.
const (
	devSecretKey = "ZGV2LW9ubHktc2VjcmV0LWtleS0zMi1ieXRlcy0hISE="
	devTokenKey  = "dev-only-token-key-change-me-0123456789abcdef"
)

// devKeys refuses the published development keys outside server.dev_mode.
func (c *Config) devKeys() []error {
	if c.Server.DevMode {
		return nil
	}
	var errs []error
	keys := append([]string{c.Security.SecretKey}, c.Security.PreviousSecretKeys...)
	for i, k := range keys {
		if sameKey(k, devSecretKey) {
			name := "security.secret_key (HANGAR_SECRET_KEY)"
			if i > 0 {
				name = "security.previous_secret_keys"
			}
			errs = append(errs, fmt.Errorf("%s is the public development key from deploy/docker-compose.yml; "+
				"generate one with `openssl rand -base64 32` (or set server.dev_mode on a development server)", name))
		}
	}
	if strings.TrimSpace(c.Docker.TokenKey) == devTokenKey {
		errs = append(errs, fmt.Errorf("docker.token_key (HANGAR_DOCKER_TOKEN_KEY) is the public development key from "+
			"deploy/docker-compose.yml; generate one with `openssl rand -base64 48` (or set server.dev_mode on a development server)"))
	}
	return errs
}

// sameKey reports whether key is the encoded secret key want, in any of the
// encodings security.secret_key accepts.
func sameKey(key, want string) bool {
	key = strings.TrimSpace(key)
	if key == "" {
		return false
	}
	if key == want {
		return true
	}
	w, err := base64.StdEncoding.DecodeString(want)
	if err != nil {
		return false
	}
	for _, dec := range []func(string) ([]byte, error){
		base64.StdEncoding.DecodeString, base64.RawStdEncoding.DecodeString,
		base64.URLEncoding.DecodeString, base64.RawURLEncoding.DecodeString, hex.DecodeString,
	} {
		if b, err := dec(key); err == nil && bytes.Equal(b, w) {
			return true
		}
	}
	return false
}
