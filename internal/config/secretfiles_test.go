package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func writeSecrets(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestSecretsDir(t *testing.T) {
	t.Setenv("HANGAR_CONFIG", "")
	dir := writeSecrets(t, map[string]string{
		"database.url":                   "postgres://hangar:pw@postgres:5432/hangar?sslmode=disable\n",
		"security.secret_key":            "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=\r\n",
		"docker.token_key":               "token-key-from-file",
		"auth.oidc.client_secret":        "oidc-from-file",
		"security.previous_secret_keys":  "ANECFHvqvLslRxQdIlP1FduLbMnvkshErgxoJMDfbJk=",
		"postgres-password":              "helper files without a dot are ignored",
		".hidden.file":                   "ignored",
		"server.trusted_proxies":         "10.0.0.5/32, 10.0.0.6/32",
		"auth.oidc.group_roles":          `{"admins":["Admin"]}`,
		"storage.s3.secret_access_key":   "s3-from-file",
		"bootstrap.admin_password":       "from-file-password-123",
		"bootstrap.admin_user":           "root-admin",
		"http.proxy_password":            "proxy-from-file",
		"observability.tracing.endpoint": "http://otel.example.com:4318",
	})
	if err := os.Mkdir(filepath.Join(dir, "sub.dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HANGAR_SECRETS_DIR", dir)
	// The environment wins over a secret file.
	t.Setenv("HANGAR_DOCKER_TOKEN_KEY", "token-key-from-env")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Database.URL != "postgres://hangar:pw@postgres:5432/hangar?sslmode=disable" {
		t.Errorf("database.url = %q", cfg.Database.URL)
	}
	if cfg.Security.SecretKey != "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=" {
		t.Errorf("security.secret_key = %q (line breaks must be trimmed)", cfg.Security.SecretKey)
	}
	if cfg.Docker.TokenKey != "token-key-from-env" {
		t.Errorf("docker.token_key = %q, want the environment value", cfg.Docker.TokenKey)
	}
	if cfg.Auth.OIDC.ClientSecret != "oidc-from-file" || cfg.Storage.S3.SecretAccessKey != "s3-from-file" ||
		cfg.HTTP.ProxyPassword != "proxy-from-file" {
		t.Errorf("secrets = %q %q %q", cfg.Auth.OIDC.ClientSecret, cfg.Storage.S3.SecretAccessKey, cfg.HTTP.ProxyPassword)
	}
	if !slices.Equal(cfg.Server.TrustedProxies, []string{"10.0.0.5/32", "10.0.0.6/32"}) {
		t.Errorf("trusted_proxies = %v", cfg.Server.TrustedProxies)
	}
	if got := cfg.Auth.OIDC.GroupRoles["admins"]; !slices.Equal(got, []string{"Admin"}) {
		t.Errorf("group_roles = %v", cfg.Auth.OIDC.GroupRoles)
	}
	if cfg.Bootstrap.AdminUser != "root-admin" || cfg.Bootstrap.AdminPassword != "from-file-password-123" {
		t.Errorf("bootstrap = %+v", cfg.Bootstrap)
	}
	if len(cfg.Security.PreviousSecretKeys) != 1 {
		t.Errorf("previous_secret_keys = %v", cfg.Security.PreviousSecretKeys)
	}
	if !slices.Contains(EnvVars(), "HANGAR_SECRETS_DIR") {
		t.Error("HANGAR_SECRETS_DIR missing from EnvVars")
	}
}

// secrets_dir in the file works like the environment variable, and a file in
// the directory overrides the YAML file.
func TestSecretsDirFromFileOverridesYAML(t *testing.T) {
	dir := writeSecrets(t, map[string]string{"database.url": "postgres://from-secret/db"})
	p := writeFile(t, "secrets_dir: "+dir+"\ndatabase:\n  url: postgres://from-yaml/db\n")
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Database.URL != "postgres://from-secret/db" {
		t.Fatalf("database.url = %q", cfg.Database.URL)
	}
}

func TestSecretsDirErrors(t *testing.T) {
	t.Setenv("HANGAR_CONFIG", "")
	t.Setenv("HANGAR_DATABASE_URL", "postgres://x/y")
	for name, tc := range map[string]struct {
		files map[string]string
		want  string
	}{
		"unknown key": {map[string]string{"security.secret_kye": "x"}, "security.secret_kye is not a setting"},
		"empty file":  {map[string]string{"docker.token_key": "\n"}, "is empty"},
		"bad value":   {map[string]string{"database.max_conns": "many"}, "database.max_conns"},
		"self":        {map[string]string{"secrets_dir": "/elsewhere"}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HANGAR_SECRETS_DIR", writeSecrets(t, tc.files))
			_, err := Load("")
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Load: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	t.Setenv("HANGAR_SECRETS_DIR", filepath.Join(t.TempDir(), "missing"))
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "secrets_dir") {
		t.Fatalf("missing dir: %v", err)
	}
}
