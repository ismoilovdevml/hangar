package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/api/catalog"
	"github.com/ismoilovdevml/hangar/internal/api/readmeimg"
	"github.com/ismoilovdevml/hangar/internal/config"
	"github.com/ismoilovdevml/hangar/internal/db"
	"github.com/ismoilovdevml/hangar/internal/secrets"
	"github.com/ismoilovdevml/hangar/internal/server"
	"github.com/ismoilovdevml/hangar/internal/service/repository"
)

// openSecrets builds the keyring from security.secret_key and seals stored
// plaintext secrets left from before encryption was configured. Without a
// key it returns a nil keyring (secrets are then refused) and warns about
// anything stored that needs one.
func openSecrets(ctx context.Context, cfg config.Config, pool *pgxpool.Pool, log *slog.Logger) (*secrets.Keyring, error) {
	keyring, err := cfg.Security.Keyring()
	if err != nil {
		return nil, err
	}
	rep, err := secrets.Sweep(ctx, pool, keyring, secrets.SweepOptions{})
	if err != nil {
		return nil, err
	}
	switch {
	case keyring == nil && rep.Plaintext > 0:
		log.Warn("stored secrets are not encrypted: set security.secret_key (HANGAR_SECRET_KEY); they are sealed on the next start",
			"plaintext", rep.Plaintext)
	case keyring == nil:
		log.Warn("security.secret_key is not set: proxy upstream credentials and blob store keys cannot be stored")
	case rep.Plaintext > 0:
		log.Info("sealed stored plaintext secrets", "count", rep.Plaintext, "key_id", keyring.PrimaryID())
	}
	if rep.Previous > 0 {
		log.Warn("secrets are sealed with a previous key: run `hangar admin rotate-secrets`", "count", rep.Previous)
	}
	for _, u := range rep.Unreadable {
		log.Error("stored secret cannot be opened", "secret", u)
	}
	return keyring, nil
}

// repoSealer is the keyring as a repository.ConfigSealer; nil without a
// key, so the service refuses configurations that carry secrets.
func repoSealer(k *secrets.Keyring) repository.ConfigSealer {
	if k == nil {
		return nil
	}
	return k
}

// storeSealer is the keyring as a catalog.SecretSealer; nil without a key.
func storeSealer(k *secrets.Keyring) catalog.SecretSealer {
	if k == nil {
		return nil
	}
	return k
}

// securityOptions maps security.* to the router's hardening options.
func securityOptions(cfg config.Config) server.Security {
	s := cfg.Security
	origins := make([]string, 0, len(s.CORS.AllowedOrigins))
	for _, o := range s.CORS.AllowedOrigins {
		if n, err := config.ParseOrigin(o); err == nil {
			origins = append(origins, n)
		}
	}
	return server.Security{
		HSTSMaxAge:            s.HSTS.MaxAge,
		HSTSIncludeSubdomains: s.HSTS.IncludeSubdomains,
		BaseURL:               cfg.Server.BaseURL,
		AllowedOrigins:        origins,
		APIRate:               server.Rate{PerMinute: s.RateLimit.API.PerMinute, Burst: s.RateLimit.API.Burst},
		FailedAuthRate:        server.Rate{PerMinute: s.RateLimit.FailedAuth.PerMinute, Burst: s.RateLimit.FailedAuth.Burst},
		APIBodyBytes:          s.Limits.APIBodyBytes,
		UploadBodyBytes:       s.Limits.UploadBodyBytes,
		BodyIdleTimeout:       s.Limits.BodyIdleTimeout,
		UIImageSources:        readmeImageSources(cfg.UI),
	}
}

// readmeImageSources are the img-src sources the web UI needs for README
// images: none when Hangar serves them, the remote schemes in direct mode.
func readmeImageSources(ui config.UI) []string {
	if ui.ReadmeImages != string(readmeimg.ModeDirect) {
		return nil
	}
	if ui.ReadmeImagesAllowHTTP {
		return []string{"https:", "http:"}
	}
	return []string{"https:"}
}

// adminRotateSecrets re-encrypts every stored secret with the primary key:
// plaintext values are sealed and values sealed with a previous key are
// re-wrapped. It exits non-zero when a value cannot be opened.
func adminRotateSecrets(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("admin rotate-secrets", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dryRun := fs.Bool("dry-run", false, "report what would change without writing")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	cfg, _, err := loadConfig(fs, args, stderr)
	if err != nil {
		return err
	}
	keyring, err := cfg.Security.Keyring()
	if err != nil {
		return err
	}
	if keyring == nil {
		return errors.New("security.secret_key (HANGAR_SECRET_KEY) is required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pool, err := db.Open(ctx, cfg.Database.URL, db.PoolOptions{MaxConns: 2})
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := ensureSchema(ctx, pool, false, slog.New(slog.DiscardHandler)); err != nil {
		return err
	}
	rep, err := secrets.Sweep(ctx, pool, keyring, secrets.SweepOptions{Rewrap: true, DryRun: *dryRun})
	if err != nil {
		return err
	}
	if *asJSON {
		if err := json.NewEncoder(stdout).Encode(rep); err != nil {
			return err
		}
	} else {
		summary := fmt.Sprintf("%d row(s) updated", rep.Rows)
		if *dryRun {
			summary = "dry run, nothing written"
		}
		_, _ = fmt.Fprintf(stdout, "primary key %s: %d plaintext sealed, %d re-wrapped, %d already current; %s\n",
			keyring.PrimaryID(), rep.Plaintext, rep.Rewrapped, rep.Current, summary)
		for _, u := range rep.Unreadable {
			_, _ = fmt.Fprintln(stdout, "cannot open:", u)
		}
	}
	return rep.Err()
}
