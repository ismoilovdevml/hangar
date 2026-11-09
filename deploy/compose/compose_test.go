// Package compose_test checks the compose stack that install.sh installs:
// bind mounts only, SELinux labels, the variables the installer writes, and
// that the server configuration it wires up loads.
package compose_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/ismoilovdevml/hangar/internal/config"
)

type mount struct {
	Type     string `yaml:"type"`
	Source   string `yaml:"source"`
	Target   string `yaml:"target"`
	ReadOnly bool   `yaml:"read_only"`
	Bind     struct {
		CreateHostPath *bool  `yaml:"create_host_path"`
		SELinux        string `yaml:"selinux"`
	} `yaml:"bind"`
}

type service struct {
	Image       string            `yaml:"image"`
	Profiles    []string          `yaml:"profiles"`
	Volumes     []mount           `yaml:"volumes"`
	Environment map[string]string `yaml:"environment"`
}

type composeFile struct {
	Name     string             `yaml:"name"`
	Volumes  map[string]any     `yaml:"volumes"`
	Services map[string]service `yaml:"services"`
}

func load(t *testing.T) (composeFile, string) {
	t.Helper()
	raw, err := os.ReadFile("compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var c composeFile
	if err := yaml.Unmarshal(raw, &c); err != nil {
		t.Fatalf("compose.yaml: %v", err)
	}
	return c, string(raw)
}

func installer(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestBindMountsOnly(t *testing.T) {
	c, _ := load(t)
	if c.Name != "hangar" {
		t.Errorf("project name %q: the migration relies on \"hangar\"", c.Name)
	}
	if len(c.Volumes) != 0 {
		t.Errorf("named volumes %v: everything must live in the data directory", c.Volumes)
	}
	for _, name := range []string{"hangar", "postgres", "caddy"} {
		if _, ok := c.Services[name]; !ok {
			t.Fatalf("service %s missing", name)
		}
	}
	type use struct{ svc, src, label string }
	var uses []use
	for name, s := range c.Services {
		for _, m := range s.Volumes {
			if m.Type != "bind" || !strings.HasPrefix(m.Source, "./") || m.Target == "" {
				t.Errorf("%s: %+v is not a bind mount of a data sub-directory", name, m)
			}
			if m.Bind.CreateHostPath == nil || *m.Bind.CreateHostPath {
				t.Errorf("%s: %s must set create_host_path: false (the installer creates it with the right owner)", name, m.Source)
			}
			if m.Bind.SELinux != "z" && m.Bind.SELinux != "Z" {
				t.Errorf("%s: %s needs an SELinux label (z or Z), got %q", name, m.Source, m.Bind.SELinux)
			}
			uses = append(uses, use{name, filepath.Clean(m.Source), m.Bind.SELinux})
		}
	}
	// A private label (Z) on a path another service also mounts would lock
	// one of them out on SELinux hosts.
	for _, a := range uses {
		for _, b := range uses {
			if a.svc == b.svc {
				continue
			}
			overlap := a.src == b.src || strings.HasPrefix(b.src, a.src+"/")
			if overlap && (a.label == "Z" || b.label == "Z") {
				t.Errorf("%s:%s and %s:%s overlap; both need the shared label z", a.svc, a.src, b.svc, b.src)
			}
		}
	}
	if p := c.Services["caddy"].Profiles; !slices.Equal(p, []string{"caddy"}) {
		t.Errorf("caddy profiles = %v", p)
	}
	if len(c.Services["postgres"].Profiles) != 0 || len(c.Services["hangar"].Profiles) != 0 {
		t.Error("hangar and postgres always run")
	}
}

// Every variable compose.yaml requires is written by install.sh, and the
// installer writes no secrets into .env.
func TestVariablesComeFromInstaller(t *testing.T) {
	_, raw := load(t)
	sh := installer(t)
	written := map[string]bool{}
	for _, m := range regexp.MustCompile(`env_set "\$env_file" ([A-Z_]+) `).FindAllStringSubmatch(sh, -1) {
		written[m[1]] = true
	}
	if len(written) < 10 {
		t.Fatalf("found only %d env_set calls in install.sh", len(written))
	}
	for _, m := range regexp.MustCompile(`\$\{([A-Z_]+)(:?[-?])?`).FindAllStringSubmatch(raw, -1) {
		name, op := m[1], m[2]
		if (op == "" || op == ":?") && !written[name] {
			t.Errorf("compose.yaml needs ${%s} but install.sh never writes it", name)
		}
	}
	for name := range written {
		if strings.Contains(name, "PASSWORD") || strings.Contains(name, "SECRET") || strings.Contains(name, "TOKEN") {
			t.Errorf("install.sh writes %s into .env; secrets belong in secrets/", name)
		}
	}
}

// The container users the installer chowns the directories to match the
// images.
func TestOwnersMatchImages(t *testing.T) {
	sh := installer(t)
	df, err := os.ReadFile("../../Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	user := regexp.MustCompile(`(?m)^USER (\d+):`).FindStringSubmatch(string(df))
	if user == nil || !strings.Contains(sh, "HANGAR_UID="+user[1]+"\n") {
		t.Errorf("Dockerfile USER %v does not match HANGAR_UID in install.sh", user)
	}
	c, _ := load(t)
	if img := c.Services["postgres"].Image; !strings.Contains(img, "-alpine") || !strings.Contains(sh, "POSTGRES_UID=70\n") {
		t.Errorf("postgres image %s: the installer assumes the alpine image (uid 70)", img)
	}
}

var interpolation = regexp.MustCompile(`\$\{([A-Z_]+)(?:(:?[-?])([^}]*))?\}`)

// interpolate resolves ${VAR}, ${VAR:-default}, ${VAR-default} and
// ${VAR:?message} like Docker Compose does.
func interpolate(s string, env map[string]string) string {
	return interpolation.ReplaceAllStringFunc(s, func(m string) string {
		p := interpolation.FindStringSubmatch(m)
		v, set := env[p[1]]
		switch p[2] {
		case ":-":
			if v == "" {
				return p[3]
			}
		case "-":
			if !set {
				return p[3]
			}
		}
		return v
	})
}

// The hangar service's environment, with the .env the installer writes and
// the secret files it generates, is a configuration the server accepts.
func TestServerConfigLoads(t *testing.T) {
	c, _ := load(t)
	h := c.Services["hangar"]
	var secretsTarget, configTarget string
	for _, m := range h.Volumes {
		switch m.Source {
		case "./secrets":
			secretsTarget = m.Target
		case "./config":
			configTarget = m.Target
			if !m.ReadOnly {
				t.Error("config/ should be mounted read-only")
			}
		}
	}
	if secretsTarget == "" || configTarget == "" {
		t.Fatal("hangar mounts no ./secrets or ./config")
	}

	dir := t.TempDir()
	secrets := filepath.Join(dir, "secrets")
	if err := os.Mkdir(secrets, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"security.secret_key":    "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=\n",
		"docker.token_key":       "dG9rZW4ta2V5LXRva2VuLWtleS10b2tlbi1rZXktdG9rZW4ta2V5LXRva2Vu\n",
		"database.url":           "postgres://hangar:pw@postgres:5432/hangar?sslmode=disable\n",
		"postgres-password":      "pw\n",
		"initial-admin-password": "abcde-fghjk-mnpqr-stuvw\n",
		"metrics.bearer_token":   "0123456789abcdef0123456789abcdef\n",
	} {
		if err := os.WriteFile(filepath.Join(secrets, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dotenv := map[string]string{
		"HANGAR_BASE_URL":         "https://hangar.example.com",
		"HANGAR_TRUSTED_PROXIES":  "10.0.0.5/32,172.30.80.0/24",
		"HANGAR_SESSION_INSECURE": "false",
	}
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "HANGAR_") {
			t.Setenv(k, "")
			_ = os.Unsetenv(k) // t.Setenv restores it
		}
	}
	for k, v := range h.Environment {
		v = interpolate(v, dotenv)
		// Container paths -> this test's directories.
		v = strings.Replace(v, secretsTarget, secrets, 1)
		if k == config.EnvConfigFile {
			if want := configTarget + "/hangar.yaml"; v != want {
				t.Errorf("HANGAR_CONFIG = %s, want %s", v, want)
			}
			v = "hangar.yaml"
		}
		t.Setenv(k, v)
	}
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("server config: %v", err)
	}
	if cfg.Server.BaseURL != "https://hangar.example.com" || !slices.Contains(cfg.Server.TrustedProxies, "10.0.0.5/32") {
		t.Errorf("server = %+v", cfg.Server)
	}
	if cfg.Database.URL != "postgres://hangar:pw@postgres:5432/hangar?sslmode=disable" || !cfg.Database.MigrateOnStart {
		t.Errorf("database = %+v", cfg.Redacted().Database)
	}
	if cfg.Security.SecretKey == "" || cfg.Docker.TokenKey == "" {
		t.Error("secret key or token key not read from secrets/")
	}
	// New installations protect /metrics with the generated token.
	if !cfg.Metrics.Enabled || cfg.Metrics.Auth != config.MetricsAuthBearer || cfg.Metrics.BearerToken != "0123456789abcdef0123456789abcdef" {
		t.Errorf("metrics = %+v, want bearer with the token of secrets/", cfg.Redacted().Metrics)
	}
	// The first administrator comes from the server's one-time password
	// file, never from environment bootstrap.
	if cfg.Bootstrap.AdminUser != "" || cfg.Bootstrap.AdminPassword != "" {
		t.Errorf("bootstrap = %+v; use the initial password file", cfg.Redacted().Bootstrap)
	}
	if cfg.Auth.Session.Insecure {
		t.Error("cookies must stay secure for an https base URL")
	}
	if cfg.Storage.Kind != "fs" || cfg.Storage.FS.Path != "/var/lib/hangar/blobs" || cfg.NATS.DataDir != "/var/lib/hangar/nats" {
		t.Errorf("storage %+v nats %+v", cfg.Storage.FS, cfg.NATS)
	}
}

// Every HANGAR_* variable the hangar service sets is a server setting, so a
// renamed setting cannot silently stop working; the one-time password lands
// in secrets/initial-admin-password, where install.sh reads it.
func TestEnvironmentIsKnown(t *testing.T) {
	c, _ := load(t)
	h := c.Services["hangar"]
	known := config.EnvVars()
	for k := range h.Environment {
		if k != config.EnvConfigFile && !slices.Contains(known, k) {
			t.Errorf("compose.yaml sets %s, which is not a server setting", k)
		}
	}
	const want = "/run/secrets/hangar/initial-admin-password"
	if got := h.Environment["HANGAR_BOOTSTRAP_INITIAL_PASSWORD_FILE"]; got != want {
		t.Errorf("HANGAR_BOOTSTRAP_INITIAL_PASSWORD_FILE = %q, want %q", got, want)
	}
	if !strings.Contains(installer(t), "pw_file=$data/secrets/initial-admin-password") {
		t.Error("install.sh does not read secrets/initial-admin-password")
	}
}

// The trivy profile: a pinned Trivy server and scan adapter on the same
// Trivy version, and the scanner settings install.sh writes into
// scanner.env make a configuration the server accepts.
func TestImageScanningWiring(t *testing.T) {
	c, raw := load(t)
	for _, name := range []string{"trivy", "hangar-scanner"} {
		s, ok := c.Services[name]
		if !ok {
			t.Fatalf("service %s missing", name)
		}
		if !slices.Equal(s.Profiles, []string{"trivy"}) {
			t.Errorf("%s profiles = %v", name, s.Profiles)
		}
		if s.Environment["HTTPS_PROXY"] == "" || !strings.Contains(s.Environment["NO_PROXY"], "hangar") {
			t.Errorf("%s: the outbound proxy is not wired (HTTPS_PROXY %q, NO_PROXY %q)", name, s.Environment["HTTPS_PROXY"],
				s.Environment["NO_PROXY"])
		}
	}
	trivyImage := regexp.MustCompile(`aquasec/trivy:(\d+\.\d+\.\d+)`)
	m := trivyImage.FindStringSubmatch(c.Services["trivy"].Image)
	if m == nil {
		t.Fatalf("trivy image %q is not pinned to a version", c.Services["trivy"].Image)
	}
	df, err := os.ReadFile("../scanner/Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	if d := trivyImage.FindStringSubmatch(string(df)); d == nil || d[1] != m[1] {
		t.Errorf("the scan adapter is built on Trivy %v, the server runs %s: keep them equal", d, m[1])
	}
	if !strings.Contains(raw, "env_file: [./scanner.env]") {
		t.Error("the hangar service does not read scanner.env")
	}

	sh := installer(t)
	env := map[string]string{}
	for _, m := range regexp.MustCompile(`echo "(HANGAR_SCANNER_[A-Z_]+)=([^"]+)"`).FindAllStringSubmatch(sh, -1) {
		env[m[1]] = m[2]
	}
	if len(env) != 3 {
		t.Fatalf("install.sh writes %v into scanner.env", env)
	}
	known := config.EnvVars()
	for k := range env {
		if !slices.Contains(known, k) {
			t.Errorf("scanner.env sets %s, which is not a server setting", k)
		}
	}
	if !strings.Contains(sh, "write_secret scanner.adapter_token") {
		t.Error("install.sh does not generate secrets/scanner.adapter_token")
	}
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "HANGAR_") {
			t.Setenv(k, "")
			_ = os.Unsetenv(k)
		}
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
	t.Setenv("HANGAR_SCANNER_ADAPTER_TOKEN", "0123456789abcdef0123456789abcdef")
	t.Setenv("HANGAR_SERVER_BASE_URL", "https://hangar.example.com")
	t.Setenv("HANGAR_DATABASE_URL", "postgres://hangar:pw@postgres:5432/hangar")
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("server config with image scanning: %v", err)
	}
	if !cfg.Scanner.ImageScanning() || cfg.Scanner.AdapterURL != "http://hangar-scanner:4955" || cfg.Scanner.RegistryURL != "http://hangar:8080" {
		t.Errorf("scanner = %+v", cfg.Redacted().Scanner)
	}
	// The adapter reaches Hangar and Trivy by their service names.
	for _, host := range []string{"hangar-scanner:4955", "trivy:4954"} {
		name, _, _ := strings.Cut(host, ":")
		if _, ok := c.Services[name]; !ok {
			t.Errorf("scanner.env points at %s, which is no service", host)
		}
	}
}
