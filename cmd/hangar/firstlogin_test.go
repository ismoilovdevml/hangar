package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/api/apitest"
	"github.com/ismoilovdevml/hangar/internal/auth"
	"github.com/ismoilovdevml/hangar/internal/auth/authtest"
	"github.com/ismoilovdevml/hangar/internal/config"
	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/observability"
)

// The generated password reaches standard error and the file, never the log
// records kept for the System logs screen.
func TestInitialAdminBannerStaysOutOfLogRing(t *testing.T) {
	file := filepath.Join(t.TempDir(), "secrets", "initial-admin-password")
	env := authtest.New(t, auth.Config{}, authtest.WithInitialPasswordFile(file))
	ring := observability.NewLogRing(observability.MinLogBufferSize)
	var stderr, banner bytes.Buffer
	log := observability.NewLoggerWithRing(&stderr, "debug", "json", ring)

	cfg := config.Default()
	cfg.Bootstrap.InitialPasswordFile = file
	if err := bootstrapFromConfig(context.Background(), cfg, env.Svc, log, &banner); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	password := strings.TrimSpace(string(b))
	if len(password) != auth.GeneratedPasswordLength {
		t.Fatalf("file holds %q", password)
	}
	if st, _ := os.Stat(file); st.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %v", st.Mode().Perm())
	}
	out := banner.String()
	for _, want := range []string{"username: admin", "password: " + password, file, "WARN ===="} {
		if !strings.Contains(out, want) {
			t.Fatalf("banner lacks %q:\n%s", want, out)
		}
	}
	page := ring.Query(observability.LogQuery{MinLevel: slog.LevelDebug, Limit: 1000})
	if len(page.Items) == 0 {
		t.Fatal("no log record about the initial administrator")
	}
	found := false
	for _, r := range page.Items {
		if strings.Contains(r.Message, "initial administrator created") {
			found = true
		}
		if strings.Contains(r.Message, password) || strings.Contains(fmtAttrs(r.Attrs), password) {
			t.Fatalf("password in the log ring: %+v", r)
		}
	}
	if !found {
		t.Fatalf("ring = %+v", page.Items)
	}
	if strings.Contains(stderr.String(), password) {
		t.Fatal("password in the structured log")
	}

	// A second start prints nothing.
	banner.Reset()
	if err := bootstrapFromConfig(context.Background(), cfg, env.Svc, log, &banner); err != nil || banner.Len() != 0 {
		t.Fatalf("second start: %v, banner %q", err, banner.String())
	}
}

func fmtAttrs(m map[string]any) string {
	var b strings.Builder
	for k, v := range m {
		b.WriteString(k)
		b.WriteString("=")
		if s, ok := v.(string); ok {
			b.WriteString(s)
		}
		b.WriteString(" ")
	}
	return b.String()
}

func TestBootstrapFromConfigRequiresChange(t *testing.T) {
	env := authtest.New(t, auth.Config{})
	cfg := config.Default()
	cfg.Bootstrap.AdminUser, cfg.Bootstrap.AdminPassword = "root", "configured passphrase"
	var banner bytes.Buffer
	if err := bootstrapFromConfig(context.Background(), cfg, env.Svc, slog.New(slog.DiscardHandler), &banner); err != nil {
		t.Fatal(err)
	}
	u, err := env.Svc.GetUserByName(context.Background(), "root")
	if err != nil || !u.MustChangePassword || banner.Len() != 0 {
		t.Fatalf("root = %+v, %v, banner %q", u, err, banner.String())
	}
}

// adminEnv points the CLI at a fully migrated schema with fast hashing.
func adminEnv(t *testing.T) *authtest.Env {
	t.Helper()
	pool := apitest.MigratedPool(t)
	t.Setenv("HANGAR_CONFIG", "")
	t.Setenv("HANGAR_DATABASE_URL", pool.Config().ConnString())
	t.Setenv("HANGAR_AUTH_ARGON2_MEMORY", "1024")
	t.Setenv("HANGAR_AUTH_ARGON2_ITERATIONS", "1")
	t.Setenv("HANGAR_AUTH_ARGON2_PARALLELISM", "1")
	return authtest.NewOnPool(t, pool, auth.Config{}, authtest.NewClock())
}

func TestAdminBootstrapAndResetPassword(t *testing.T) {
	env := adminEnv(t)
	ctx := context.Background()
	var out, errb bytes.Buffer
	if code := run([]string{"admin", "bootstrap", "--username", "admin", "--password-stdin"},
		strings.NewReader("a bootstrap passphrase\n"), &out, &errb); code != 0 {
		t.Fatalf("bootstrap: %d %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "must set a new password") {
		t.Fatalf("bootstrap output = %q", out.String())
	}
	if u, err := env.Svc.GetUserByName(ctx, "admin"); err != nil || !u.MustChangePassword {
		t.Fatalf("admin = %+v, %v", u, err)
	}

	out.Reset()
	errb.Reset()
	if code := run([]string{"admin", "reset-password", "--user", "admin"}, nil, &out, &errb); code != 0 {
		t.Fatalf("reset-password: %d %s", code, errb.String())
	}
	password := strings.TrimSpace(out.String())
	if len(password) != auth.GeneratedPasswordLength || !strings.Contains(errb.String(), "one-time password for admin") {
		t.Fatalf("reset-password printed %q / %q", out.String(), errb.String())
	}
	p, err := env.Svc.Login(ctx, "admin", password)
	if err != nil || !p.PasswordChangeRequired {
		t.Fatalf("login with the one-time password = %+v, %v", p, err)
	}
	if _, err := env.Svc.Login(ctx, "admin", "a bootstrap passphrase"); err == nil {
		t.Fatal("the old password still works")
	}

	// Tokens cannot be minted for the account from the CLI either.
	errb.Reset()
	if code := run([]string{"admin", "token", "create", "--user", "admin", "--name", "ci", "--scopes", "repo:*:read"}, nil, &out, &errb); code == 0 ||
		!strings.Contains(errb.String(), "new password") {
		t.Fatalf("token create: %d %q", code, errb.String())
	}
	if err := env.Svc.ChangePassword(core.WithPrincipal(ctx, p), password, "my chosen passphrase", ""); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := run([]string{"admin", "token", "create", "--user", "admin", "--name", "ci", "--scopes", "repo:*:read"}, nil, &out, &errb); code != 0 {
		t.Fatalf("token create after the change: %d %s", code, errb.String())
	}

	errb.Reset()
	if code := run([]string{"admin", "reset-password", "--user", "nobody"}, nil, &out, &errb); code == 0 {
		t.Fatal("reset-password of an unknown user succeeded")
	}
	if code := run([]string{"admin", "reset-password"}, nil, &out, &errb); code == 0 {
		t.Fatal("reset-password without --user succeeded")
	}
}
