package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"

	"github.com/ismoilovdevml/hangar/internal/db"
)

func migrateCmd(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fs.Usage = func() {
		_, _ = fmt.Fprintln(fs.Output(), "usage: hangar migrate [--config FILE] up|down|status|version")
		fs.PrintDefaults()
	}
	cfg, log, err := loadConfig(fs, args, stderr)
	if err != nil {
		return err
	}
	action := "up"
	if fs.NArg() > 0 {
		action = fs.Arg(0)
	}
	if fs.NArg() > 1 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args()[1:])
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pool, err := db.Open(ctx, cfg.Database.URL, db.PoolOptions{MaxConns: 2})
	if err != nil {
		return err
	}
	defer pool.Close()
	m, err := db.NewMigrator(pool)
	if err != nil {
		return err
	}
	defer func() { _ = m.Close() }()

	switch action {
	case "up":
		return m.Up(ctx, log)
	case "down":
		return m.Down(ctx, log)
	case "version":
		v, err := m.Version(ctx)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(stdout, v)
		return err
	case "status":
		st, err := m.Status(ctx)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "VERSION\tSTATE\tAPPLIED AT\tFILE")
		for _, s := range st {
			state, at := "pending", "-"
			if s.Applied {
				state, at = "applied", s.AppliedAt.UTC().Format("2006-01-02 15:04:05")
			}
			_, _ = fmt.Fprintf(tw, "%d\t%s\t%s\t%s\n", s.Version, state, at, s.Name)
		}
		return tw.Flush()
	default:
		fs.Usage()
		return fmt.Errorf("unknown migrate action %q", action)
	}
}
