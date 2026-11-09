package config

import (
	"encoding/hex"
	"os"
	"regexp"
	"testing"
)

// TestDevKeysRefused: the keys published in deploy/docker-compose.yml only
// start a server that says it is a development server.
func TestDevKeysRefused(t *testing.T) {
	hexDev := hex.EncodeToString([]byte("dev-only-secret-key-32-bytes-!!!"))
	for name, env := range map[string]map[string]string{
		"secret key":          {"HANGAR_SECRET_KEY": devSecretKey},
		"secret key as hex":   {"HANGAR_SECRET_KEY": hexDev},
		"token key":           {"HANGAR_DOCKER_TOKEN_KEY": devTokenKey},
		"previous secret key": {"HANGAR_SECRET_KEY": "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=", "HANGAR_SECURITY_PREVIOUS_SECRET_KEYS": devSecretKey},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadWith(t, env)
			wantErr(t, err, "public development key", "server.dev_mode")
			env["HANGAR_SERVER_DEV_MODE"] = "true"
			if _, err := loadWith(t, env); err != nil {
				t.Fatalf("with server.dev_mode: %v", err)
			}
		})
	}
	if _, err := loadWith(t, map[string]string{"HANGAR_SECRET_KEY": "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=",
		"HANGAR_DOCKER_TOKEN_KEY": "a-real-random-token-key-of-enough-length-0001"}); err != nil {
		t.Fatalf("own keys: %v", err)
	}
}

// TestDevComposeMatchesDevKeys: the development compose file sets dev_mode
// and its default keys are the ones refused elsewhere.
func TestDevComposeMatchesDevKeys(t *testing.T) {
	b, err := os.ReadFile("../../deploy/docker-compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	for _, re := range []string{
		`HANGAR_SERVER_DEV_MODE: "true"`,
		`HANGAR_SECRET_KEY: \$\{HANGAR_SECRET_KEY:-` + regexp.QuoteMeta(devSecretKey) + `\}`,
		`HANGAR_DOCKER_TOKEN_KEY: \$\{HANGAR_DOCKER_TOKEN_KEY:-` + regexp.QuoteMeta(devTokenKey) + `\}`,
	} {
		if !regexp.MustCompile(re).Match(b) {
			t.Errorf("deploy/docker-compose.yml does not match %s", re)
		}
	}
}
