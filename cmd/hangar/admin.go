package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ismoilovdevml/hangar/internal/auth"
	"github.com/ismoilovdevml/hangar/internal/config"
	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/db"
)

const adminUsage = `usage:
  hangar admin bootstrap --username NAME --password-stdin [--config FILE]
        create the first administrator (no-op when an administrator exists)
  hangar admin token create --user NAME --name LABEL --scopes LIST [--expires 90d] [--config FILE]
        create a personal access token for NAME and print it once
        LIST is comma-separated, e.g. "repo:*:read,repo:*:write"
`

// adminCmd runs administrative commands directly against the database, for
// first-run setup and automation (no running server needed).
func adminCmd(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	switch {
	case len(args) >= 1 && args[0] == "bootstrap":
		return adminBootstrap(args[1:], stdin, stdout, stderr)
	case len(args) >= 2 && args[0] == "token" && args[1] == "create":
		return adminTokenCreate(args[2:], stdout, stderr)
	case len(args) >= 1 && (args[0] == "help" || args[0] == "-h" || args[0] == "--help"):
		_, _ = fmt.Fprint(stdout, adminUsage)
		return nil
	default:
		_, _ = fmt.Fprint(stderr, adminUsage)
		return errors.New("unknown admin command")
	}
}

// withAuth opens the database (schema must be current) and the auth service,
// runs fn and closes everything, flushing audit records.
func withAuth(cfg config.Config, log *slog.Logger, fn func(ctx context.Context, svc *auth.Service) error) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pool, err := db.Open(ctx, cfg.Database.URL, db.PoolOptions{MaxConns: 4})
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := ensureSchema(ctx, pool, false, log); err != nil {
		return err
	}
	auditLog, svc, err := openAuth(cfg, pool, nil, log)
	if err != nil {
		return err
	}
	defer closeAudit(auditLog, log)
	defer svc.Close()
	return fn(ctx, svc)
}

func adminBootstrap(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("admin bootstrap", flag.ContinueOnError)
	fs.SetOutput(stderr)
	username := fs.String("username", "", "administrator user name (required)")
	fromStdin := fs.Bool("password-stdin", false, "read the password from standard input (required)")
	cfg, log, err := loadConfig(fs, args, stderr)
	if err != nil {
		return err
	}
	if *username == "" || !*fromStdin {
		fs.Usage()
		return errors.New("--username and --password-stdin are required")
	}
	password, err := readSecret(stdin)
	if err != nil {
		return err
	}
	return withAuth(cfg, log, func(ctx context.Context, svc *auth.Service) error {
		created, err := svc.EnsureBootstrapAdmin(ctx, *username, password)
		if err != nil {
			return err
		}
		if created {
			_, err = fmt.Fprintf(stdout, "administrator %q is ready\n", *username)
		} else {
			_, err = fmt.Fprintln(stdout, "an administrator already exists; nothing changed")
		}
		return err
	})
}

func adminTokenCreate(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("admin token create", flag.ContinueOnError)
	fs.SetOutput(stderr)
	user := fs.String("user", "", "owner of the token (required)")
	name := fs.String("name", "", "token label (required)")
	scopes := fs.String("scopes", "", `comma-separated privileges, e.g. "repo:*:read,repo:*:write" (required)`)
	expires := fs.String("expires", "90d", "lifetime: Go duration or days (30d)")
	cfg, log, err := loadConfig(fs, args, stderr)
	if err != nil {
		return err
	}
	if *user == "" || *name == "" || *scopes == "" {
		fs.Usage()
		return errors.New("--user, --name and --scopes are required")
	}
	var privs []core.Privilege
	for s := range strings.SplitSeq(*scopes, ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		p, err := core.ParsePrivilege(s)
		if err != nil {
			return err
		}
		privs = append(privs, p)
	}
	ttl, err := parseLifetime(*expires)
	if err != nil {
		return err
	}
	return withAuth(cfg, log, func(ctx context.Context, svc *auth.Service) error {
		u, err := svc.GetUserByName(ctx, *user)
		if err != nil {
			return fmt.Errorf("user %q: %w", *user, err)
		}
		if u.Disabled {
			return fmt.Errorf("user %q is disabled", *user)
		}
		kind := core.PrincipalUser
		if u.Kind == auth.KindServiceAccount {
			kind = core.PrincipalServiceAccount
		}
		ctx = core.WithPrincipal(ctx, &core.Principal{ID: u.ID, Name: u.Username, Kind: kind, Source: u.Source, Roles: u.Roles})
		tok, secret, err := svc.CreateToken(ctx, auth.TokenInput{Name: *name, Scopes: privs, ExpiresAt: time.Now().Add(ttl)})
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stderr, "token %q for %s created, expires %s (shown once)\n", tok.Name, tok.Owner, tok.ExpiresAt.Format(time.RFC3339))
		_, err = fmt.Fprintln(stdout, secret)
		return err
	})
}

// parseLifetime accepts a Go duration ("720h") or a number of days ("30d").
func parseLifetime(s string) (time.Duration, error) {
	if d, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(d)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("--expires %q: want a positive number of days", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("--expires %q: want a positive duration such as 720h or 30d", s)
	}
	return d, nil
}

// readSecret reads one line (without the line break) from r.
func readSecret(r io.Reader) (string, error) {
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return "", errors.New("empty password on standard input")
	}
	return line, nil
}
