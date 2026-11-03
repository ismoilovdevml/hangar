package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestMain clears HANGAR_* variables inherited from the caller's shell so
// results do not depend on the developer's environment.
func TestMain(m *testing.M) {
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); strings.HasPrefix(name, EnvPrefix) {
			_ = os.Unsetenv(name)
		}
	}
	os.Exit(m.Run())
}

func writeFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "hangar.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadFileThenEnv(t *testing.T) {
	p := writeFile(t, `
server:
  addr: ":9000"
  base_url: "https://hangar.example.com/"
  shutdown_timeout: 5s
database:
  url: postgres://file@db/hangar
storage:
  kind: fs
  fs:
    path: /var/lib/hangar/blobs
nats:
  embedded: true
  data_dir: /var/lib/hangar/nats
`)
	t.Setenv("HANGAR_DATABASE_URL", "postgres://env@db/hangar")
	t.Setenv("HANGAR_LOG_LEVEL", "debug")
	t.Setenv("HANGAR_SERVER_TRUSTED_PROXIES", "10.0.0.0/8, 192.168.0.0/16")
	t.Setenv("HANGAR_DATABASE_MAX_CONNS", "7")
	t.Setenv("HANGAR_CONFIG", "")

	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Addr != ":9000" || cfg.Server.ShutdownTimeout != 5*time.Second {
		t.Errorf("server = %+v", cfg.Server)
	}
	if cfg.Server.BaseURL != "https://hangar.example.com" {
		t.Errorf("base_url not trimmed: %q", cfg.Server.BaseURL)
	}
	if cfg.Database.URL != "postgres://env@db/hangar" || cfg.Database.MaxConns != 7 {
		t.Errorf("env did not override database: %+v", cfg.Database)
	}
	if cfg.Log.Level != "debug" || cfg.Log.Format != "json" {
		t.Errorf("log = %+v", cfg.Log)
	}
	if !slices.Equal(cfg.Server.TrustedProxies, []string{"10.0.0.0/8", "192.168.0.0/16"}) {
		t.Errorf("trusted proxies = %v", cfg.Server.TrustedProxies)
	}
	if cfg.Storage.FS.Path != "/var/lib/hangar/blobs" {
		t.Errorf("storage = %+v", cfg.Storage)
	}
}

func TestLoadEnvOnly(t *testing.T) {
	t.Setenv("HANGAR_CONFIG", "")
	t.Setenv("HANGAR_DATABASE_URL", "postgres://u:secret@localhost/h")
	t.Setenv("HANGAR_NATS_EMBEDDED", "false")
	t.Setenv("HANGAR_NATS_URL", "nats://nats:4222")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.NATS.Embedded || cfg.NATS.URL != "nats://nats:4222" {
		t.Errorf("nats = %+v", cfg.NATS)
	}
	if r := cfg.Redacted(); strings.Contains(r.Database.URL, "secret") {
		t.Errorf("Redacted leaked password: %s", r.Database.URL)
	}
}

func TestValidationErrors(t *testing.T) {
	t.Setenv("HANGAR_CONFIG", "")
	t.Setenv("HANGAR_DATABASE_URL", "")
	t.Setenv("HANGAR_STORAGE_KIND", "tape")
	_, err := Load("")
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"database.url", "storage.kind"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

func TestUnknownKeyAndBadEnv(t *testing.T) {
	t.Setenv("HANGAR_CONFIG", "")
	t.Setenv("HANGAR_DATABASE_URL", "postgres://x/y")
	if _, err := Load(writeFile(t, "server:\n  adr: ':1'\n")); err == nil {
		t.Error("unknown key must fail (typo protection)")
	}
	t.Setenv("HANGAR_SERVER_IDLE_TIMEOUT", "forever")
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "HANGAR_SERVER_IDLE_TIMEOUT") {
		t.Errorf("bad duration error = %v", err)
	}
}

func TestEmptyFileAndMissingFile(t *testing.T) {
	t.Setenv("HANGAR_CONFIG", "")
	t.Setenv("HANGAR_DATABASE_URL", "postgres://x/y")
	if _, err := Load(writeFile(t, "")); err != nil {
		t.Errorf("empty file: %v", err)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Error("missing file must fail")
	}
}

func TestHTTPAndTrustedProxies(t *testing.T) {
	t.Setenv("HANGAR_CONFIG", "")
	t.Setenv("HANGAR_DATABASE_URL", "postgres://x/y")
	t.Setenv("HANGAR_HTTP_PROXY", "http://proxy.corp:3128")
	t.Setenv("HANGAR_HTTP_PROXY_PASSWORD", "s3cret")
	t.Setenv("HANGAR_HTTP_NO_PROXY", ".corp.example,10.0.0.0/8")
	t.Setenv("HANGAR_SERVER_TRUSTED_PROXIES", "10.0.0.0/8,192.168.1.10")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTP.Proxy != "http://proxy.corp:3128" || len(cfg.HTTP.NoProxy) != 2 || !cfg.HTTP.UseEnvironment {
		t.Errorf("http = %+v", cfg.HTTP)
	}
	if strings.Contains(cfg.Redacted().HTTP.ProxyPassword, "s3cret") {
		t.Error("proxy password not redacted")
	}
	t.Setenv("HANGAR_HTTP_PROXY", "ftp://proxy:21")
	t.Setenv("HANGAR_SERVER_TRUSTED_PROXIES", "not-a-cidr")
	_, err = Load("")
	if err == nil || !strings.Contains(err.Error(), "http.proxy") || !strings.Contains(err.Error(), "trusted_proxies") {
		t.Errorf("validation = %v", err)
	}
}

func TestEnvVars(t *testing.T) {
	vars := EnvVars()
	for _, want := range []string{"HANGAR_DATABASE_URL", "HANGAR_STORAGE_S3_SECRET_ACCESS_KEY", "HANGAR_NATS_EMBEDDED", "HANGAR_SERVER_SHUTDOWN_TIMEOUT"} {
		if !slices.Contains(vars, want) {
			t.Errorf("missing %s in %v", want, vars)
		}
	}
}

// The example config shipped at the repository root must load.
func TestExampleConfig(t *testing.T) {
	t.Setenv("HANGAR_CONFIG", "")
	if _, err := Load("../../hangar.example.yaml"); err != nil {
		t.Fatalf("hangar.example.yaml: %v", err)
	}
}
