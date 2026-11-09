package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// loadWith loads the configuration from env alone: HANGAR_* variables set
// by earlier calls in the same test are removed first.
func loadWith(t *testing.T, env map[string]string) (Config, error) {
	t.Helper()
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); strings.HasPrefix(name, EnvPrefix) {
			t.Setenv(name, "") // restored after the test
			_ = os.Unsetenv(name)
		}
	}
	t.Setenv("HANGAR_DATABASE_URL", "postgres://x/y")
	for k, v := range env {
		t.Setenv(k, v)
	}
	return Load("")
}

func wantErr(t *testing.T, err error, parts ...string) {
	t.Helper()
	for _, p := range parts {
		if err == nil || !strings.Contains(err.Error(), p) {
			t.Errorf("error %v does not mention %q", err, p)
		}
	}
}

func TestNATSReplicas(t *testing.T) {
	cfg, err := loadWith(t, nil)
	if err != nil || cfg.NATS.Replicas != 1 {
		t.Fatalf("default replicas = %d, %v", cfg.NATS.Replicas, err)
	}
	cluster := map[string]string{"HANGAR_NATS_EMBEDDED": "false", "HANGAR_NATS_URL": "nats://nats:4222"}
	for _, n := range []string{"1", "3", "5"} {
		cluster["HANGAR_NATS_REPLICAS"] = n
		if _, err := loadWith(t, cluster); err != nil {
			t.Errorf("replicas %s on a cluster: %v", n, err)
		}
	}
	for _, n := range []string{"0", "6", "-1"} {
		cluster["HANGAR_NATS_REPLICAS"] = n
		_, err := loadWith(t, cluster)
		wantErr(t, err, "nats.replicas "+n, "want 1 to 5")
	}
	_, err = loadWith(t, map[string]string{"HANGAR_NATS_REPLICAS": "3"})
	wantErr(t, err, "embedded NATS is a single server")
}

func TestShutdownSettings(t *testing.T) {
	cfg, err := loadWith(t, nil)
	if err != nil || cfg.Server.DrainDelay != 0 || cfg.Server.ShutdownTimeout != 30*time.Second {
		t.Fatalf("defaults drain=%s shutdown=%s, %v", cfg.Server.DrainDelay, cfg.Server.ShutdownTimeout, err)
	}
	cfg, err = loadWith(t, map[string]string{"HANGAR_SERVER_DRAIN_DELAY": "15s", "HANGAR_SERVER_SHUTDOWN_TIMEOUT": "2m"})
	if err != nil || cfg.Server.DrainDelay != 15*time.Second || cfg.Server.ShutdownTimeout != 2*time.Minute {
		t.Fatalf("parsed drain=%s shutdown=%s, %v", cfg.Server.DrainDelay, cfg.Server.ShutdownTimeout, err)
	}
	_, err = loadWith(t, map[string]string{"HANGAR_SERVER_DRAIN_DELAY": "6m", "HANGAR_SERVER_SHUTDOWN_TIMEOUT": "0s"})
	wantErr(t, err, "server.drain_delay 6m0s: want 0 to 5m0s", "server.shutdown_timeout 0s: want 1s to 24h")
	_, err = loadWith(t, map[string]string{"HANGAR_SERVER_DRAIN_DELAY": "-1s"})
	wantErr(t, err, "server.drain_delay -1s")
}

func TestDatabaseMaxConnsForMigrationLock(t *testing.T) {
	_, err := loadWith(t, map[string]string{"HANGAR_DATABASE_MAX_CONNS": "1"})
	wantErr(t, err, "database.max_conns 1: want at least 2")
}

func TestMetricsAuthSettings(t *testing.T) {
	const token = "0123456789abcdef-token"
	cfg, err := loadWith(t, nil)
	if err != nil || cfg.Metrics.Auth != MetricsAuthNone {
		t.Fatalf("default auth = %q, %v", cfg.Metrics.Auth, err)
	}

	cfg, err = loadWith(t, map[string]string{"HANGAR_METRICS_AUTH": "Bearer", "HANGAR_METRICS_BEARER_TOKEN": token,
		"HANGAR_METRICS_ALLOWED_CIDRS": "10.0.0.0/8, 192.0.2.7"})
	if err != nil {
		t.Fatal(err)
	}
	if m := cfg.Metrics; m.Auth != MetricsAuthBearer || m.BearerToken != token || len(m.AllowedCIDRs) != 2 {
		t.Errorf("bearer = %+v", m)
	}
	if r := cfg.Redacted(); strings.Contains(r.Metrics.BearerToken, token) {
		t.Error("bearer token not redacted")
	}

	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	pwFile := filepath.Join(dir, "password")
	if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pwFile, []byte("correct-horse-battery\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = loadWith(t, map[string]string{"HANGAR_METRICS_AUTH": "bearer", "HANGAR_METRICS_BEARER_TOKEN_FILE": tokenFile})
	if err != nil || cfg.Metrics.BearerToken != token {
		t.Fatalf("token from file = %q, %v", cfg.Metrics.BearerToken, err)
	}
	cfg, err = loadWith(t, map[string]string{"HANGAR_METRICS_AUTH": "basic", "HANGAR_METRICS_USERNAME": "prometheus",
		"HANGAR_METRICS_PASSWORD_FILE": pwFile})
	if err != nil || cfg.Metrics.Password != "correct-horse-battery" {
		t.Fatalf("password from file = %q, %v", cfg.Metrics.Password, err)
	}
	if r := cfg.Redacted(); r.Metrics.Password != "REDACTED" {
		t.Error("password not redacted")
	}

	for _, tc := range []struct {
		name string
		env  map[string]string
		want []string
	}{
		{"unknown mode", map[string]string{"HANGAR_METRICS_AUTH": "mtls"}, []string{`metrics.auth "mtls"`}},
		{"bearer without token", map[string]string{"HANGAR_METRICS_AUTH": "bearer"}, []string{"metrics.auth=bearer needs metrics.bearer_token"}},
		{"short token", map[string]string{"HANGAR_METRICS_AUTH": "bearer", "HANGAR_METRICS_BEARER_TOKEN": "short"}, []string{"at least 16 characters"}},
		{"token and file", map[string]string{"HANGAR_METRICS_AUTH": "bearer", "HANGAR_METRICS_BEARER_TOKEN": token,
			"HANGAR_METRICS_BEARER_TOKEN_FILE": tokenFile}, []string{"both set"}},
		{"missing file", map[string]string{"HANGAR_METRICS_AUTH": "bearer", "HANGAR_METRICS_BEARER_TOKEN_FILE": filepath.Join(dir, "nope")},
			[]string{"metrics.bearer_token_file"}},
		{"basic without user", map[string]string{"HANGAR_METRICS_AUTH": "basic", "HANGAR_METRICS_PASSWORD": "correct-horse-battery"},
			[]string{"needs metrics.username"}},
		{"basic without password", map[string]string{"HANGAR_METRICS_AUTH": "basic", "HANGAR_METRICS_USERNAME": "prometheus"},
			[]string{"needs metrics.password"}},
		{"credentials without auth", map[string]string{"HANGAR_METRICS_BEARER_TOKEN": token}, []string{"metrics.auth is none"}},
		{"bad cidr", map[string]string{"HANGAR_METRICS_ALLOWED_CIDRS": "10.0.0.0/33"}, []string{`metrics.allowed_cidrs: "10.0.0.0/33"`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadWith(t, tc.env)
			wantErr(t, err, tc.want...)
		})
	}
}
